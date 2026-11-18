package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"lastseen/internal/logx"
)

// External bot listings (top.gg, discordbotlist.com, voidbots.net) and the
// stats.json push to the LAN webserver. Each is a no-op unless configured;
// failures are logged and swallowed so an outage never disrupts the bot.

var (
	topggLog    = logx.Get("topgg")
	dblLog      = logx.Get("discordbotlist")
	voidLog     = logx.Get("voidbots")
	statsLog    = logx.Get("stats-push")
	listingHTTP = &http.Client{Timeout: 60 * time.Second}
)

const (
	topggMetricsURL = "https://top.gg/api/v1/projects/@me/metrics"
	dblStatsURL     = "https://discordbotlist.com/api/v1/bots/%s/stats"
	voidStatsURL    = "https://api.voidbots.net/bot/stats/%s"
	statsLocal      = "stats.json"
)

// listingsDelay is how long guild joins and leaves are collected before the
// listings get one update: Discord can remove several guilds in the same
// second, and VoidBots accepts one post per 3 minutes.
const listingsDelay = 10 * time.Second

// scheduleListingsUpdate updates the listings once no guild has joined or
// left for listingsDelay.
func (b *Bot) scheduleListingsUpdate() {
	b.listingsMu.Lock()
	defer b.listingsMu.Unlock()
	if b.listingsTimer == nil {
		b.listingsTimer = time.AfterFunc(listingsDelay, func() {
			defer recoverLog("listings update")
			b.updateListings()
		})
		return
	}
	b.listingsTimer.Reset(listingsDelay) // also re-arms it after it has fired
}

// stopListingsUpdate drops a pending update (shutdown).
func (b *Bot) stopListingsUpdate() {
	b.listingsMu.Lock()
	defer b.listingsMu.Unlock()
	if b.listingsTimer != nil {
		b.listingsTimer.Stop()
	}
}

// updateListings pushes the current counts to every configured listing.
func (b *Bot) updateListings() {
	b.updateTopGG()
	b.updateDiscordBotList()
	b.updateVoidBots()
}

func (b *Bot) selfID() string {
	if u, ok := b.Client.Caches.SelfUser(); ok {
		return u.ID.String()
	}
	return b.Client.ApplicationID.String()
}

// postJSON sends payload and returns the status code and body.
func postJSON(method, url, auth string, payload any) (int, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := listingHTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return resp.StatusCode, string(text), nil
}

func (b *Bot) updateTopGG() {
	token := b.Cfg.TopGGToken
	if token == "" {
		return
	}
	servers, shards := len(b.guildIDs()), b.shardCount
	status, body, err := postJSON(http.MethodPatch, topggMetricsURL, "Bearer "+token,
		map[string]int{"server_count": servers, "shard_count": shards})
	switch {
	case err != nil:
		topggLog.Error("Error updating TOP.GG metrics: %v", err)
	case status >= 200 && status < 300:
		topggLog.Info("Updated TOP.GG metrics: %d servers, %d shard(s)", servers, shards)
	default:
		topggLog.Warn("TOP.GG metrics update failed (%d): %s", status, body)
	}
}

func (b *Bot) updateDiscordBotList() {
	token := b.Cfg.DiscordBotListToken
	if token == "" {
		return
	}
	// Site vanity slug (e.g. "lastseen-2591"); the numeric ID is rejected.
	botID := b.Cfg.DiscordBotListBotID
	if botID == "" {
		botID = b.selfID()
	}
	guilds := b.guildIDs()
	users := 0
	for _, id := range guilds {
		if g, ok := b.Client.Caches.Guild(id); ok {
			users += g.MemberCount
		}
	}
	status, body, err := postJSON(http.MethodPost, fmt.Sprintf(dblStatsURL, botID), token,
		map[string]int{"guilds": len(guilds), "users": users})
	switch {
	case err != nil:
		dblLog.Error("Error updating DiscordBotList metrics: %v", err)
	case status >= 200 && status < 300:
		dblLog.Info("Updated DiscordBotList metrics: %d guilds, %d users", len(guilds), users)
	default:
		dblLog.Warn("DiscordBotList metrics update failed (%d): %s", status, body)
	}
}

func (b *Bot) updateVoidBots() {
	token := b.Cfg.VoidBotsToken
	if token == "" {
		return
	}
	servers, shards := len(b.guildIDs()), b.shardCount
	status, body, err := postJSON(http.MethodPost, fmt.Sprintf(voidStatsURL, b.selfID()), token,
		map[string]int{"server_count": servers, "shard_count": shards})
	switch {
	case err != nil:
		voidLog.Error("Error updating VoidBots metrics: %v", err)
	case status >= 200 && status < 300:
		voidLog.Info("Updated VoidBots metrics: %d servers, %d shard(s)", servers, shards)
	default:
		voidLog.Warn("VoidBots metrics update failed (%d): %s", status, body)
	}
}

// pushStats writes stats.json (the /about counts) and copies it to the
// webserver with scp. Only when STATS_PUSH_ENABLED is set.
func (b *Bot) pushStats() {
	if !b.Cfg.StatsPushEnabled {
		return
	}
	dest, key := b.Cfg.StatsSCPDestination, b.Cfg.StatsSSHKey
	if dest == "" || key == "" {
		statsLog.Warn("Stats push enabled but STATS_SCP_DESTINATION or STATS_SSH_KEY is unset; skipping")
		return
	}
	stats := b.DB.GetBotStatistics()
	updated := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	// The file the website reads: server and user counts, and when (UTC).
	data := fmt.Sprintf(`{"servers": %d, "users": %d, "updated": "%s"}`, stats.TotalGuilds, stats.TotalUsers, updated)
	if err := os.WriteFile(statsLocal, []byte(data), 0o644); err != nil {
		statsLog.Error("Failed to write %s: %v", statsLocal, err)
		return
	}
	// Bounded, and BatchMode fails instead of waiting on a prompt.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "scp", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-i", key, statsLocal, dest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		statsLog.Info("Pushed stats to webserver: %d servers, %d users", stats.TotalGuilds, stats.TotalUsers)
		return
	}
	if ee, ok := err.(*exec.ExitError); ok && ctx.Err() == nil {
		statsLog.Warn("stats scp failed (exit %d): %s", ee.ExitCode(), strings.TrimSpace(stderr.String()))
		return
	}
	statsLog.Error("Error pushing stats to webserver: %v", err)
}
