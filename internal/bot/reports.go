package bot

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"lastseen/internal/db"
	"lastseen/internal/locale"
	"lastseen/internal/logx"
	"lastseen/internal/textfmt"
)

var reportLog = logx.Get("reports")

// reportRateLimitWindow is the minimum gap between two reports to one guild.
const reportRateLimitWindow = 60 * time.Second

// reportState serialises report sends per guild and spaces them out.
type reportState struct {
	mu       sync.Mutex
	locks    map[snowflake.ID]*sync.Mutex
	lastSend map[snowflake.ID]time.Time
}

var reports = &reportState{locks: map[snowflake.ID]*sync.Mutex{}, lastSend: map[snowflake.ID]time.Time{}}

func (r *reportState) lock(guildID snowflake.ID) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.locks[guildID]
	if !ok {
		l = &sync.Mutex{}
		r.locks[guildID] = l
	}
	return l
}

// purgeGuildState drops the per-guild report state of a removed guild.
func purgeGuildState(guildID snowflake.ID) {
	reports.mu.Lock()
	delete(reports.locks, guildID)
	delete(reports.lastSend, guildID)
	reports.mu.Unlock()
}

// guildLang is guild_language(config): the guild's language, else the default.
func guildLang(cfg *db.GuildConfig) string {
	if cfg == nil {
		return locale.Default
	}
	return locale.GuildLanguage(cfg.Language)
}

func hourLabel(h int) string { return fmt.Sprintf("%02d:00", h) }

// generateActivityReport builds the scheduled report embed (a dozen-plus DB
// reads; runs off the gateway goroutine).
func (b *Bot) generateActivityReport(guildID snowflake.ID, guildName string, days int, reportTypes []string) discord.Embed {
	gid := i64(guildID)
	cfg := b.DB.GetGuildConfig(gid)
	lang := guildLang(cfg)
	tz := "UTC"
	if cfg != nil {
		tz = cfg.Timezone
	}

	tracked := b.DB.GetTrackedUserIDs(gid)
	var trackedSet map[int64]bool
	if tracked != nil {
		trackedSet = make(map[int64]bool, len(tracked.IDs))
		for _, id := range tracked.IDs {
			trackedSet[id] = true
		}
	}

	stats := b.DB.GetGuildMessageActivityStats(gid, days, tracked)
	topUsers := b.DB.GetTopActiveUsersPeriod(gid, days, 5, tracked)
	byDay, dayOK := b.DB.GetActivityByDay(gid, days, tracked, tz)

	titleKey := "report.title.monthly"
	if days == 7 {
		titleKey = "report.title.weekly"
	}
	e := newEmbed(t(titleKey, lang, "guild", guildName), ColorBlue)

	var total int64
	switch {
	case days <= 7:
		total = stats.Total7d
	case days <= 30:
		total = stats.Total30d
	case days <= 90:
		total = stats.Total90d
	default:
		total = stats.Total365d
	}
	desc := t("report.activity_header", lang, "days", days)
	desc += t("report.total_messages", lang, "total", total)
	desc += t("report.daily_average", lang, "avg", stats.AvgPerDay)

	if dayOK {
		peak := 0
		for i := 1; i < 7; i++ {
			if byDay[i] > byDay[peak] {
				peak = i
			}
		}
		desc += t("report.peak_day", lang, "day", locale.Weekday(peak, lang), "count", byDay[peak])
	}

	w := b.DB.GetServerActivityWindows(gid, days, tz, tracked)
	if w.PeakHour != nil && w.ByHour[*w.PeakHour] > 0 {
		ph := *w.PeakHour
		label := hourLabel(ph) + "-" + hourLabel((ph+1)%24)
		desc += t("report.peak_hour", lang, "hour", label, "tz", tz, "count", w.ByHour[ph])
	}
	desc += "\n"

	if w.Total >= 50 && w.ActiveDays >= 7 && w.Recommend != nil {
		r := *w.Recommend
		desc += t("report.best_time_header", lang)
		desc += t("report.best_time_line", lang, "day", locale.Weekday(r[0], lang),
			"start", hourLabel(r[1]), "end", hourLabel(r[2]%24), "tz", tz)
		if w.Quiet != nil {
			q := *w.Quiet
			desc += t("report.best_time_quiet", lang, "day", locale.Weekday(q[0], lang),
				"start", hourLabel(q[1]), "end", hourLabel(q[2]%24))
		}
		desc += "\n"
	}

	showJoined, showLeft := contains(reportTypes, "members"), contains(reportTypes, "departures")
	if showJoined || showLeft {
		desc += t("report.member_changes_header", lang)
		inScope := func(ms []db.PeriodMember) int {
			if trackedSet == nil {
				return len(ms)
			}
			n := 0
			for _, m := range ms {
				if trackedSet[m.UserID] {
					n++
				}
			}
			return n
		}
		var joined, left int
		if showJoined {
			joined = inScope(b.DB.GetNewMembersPeriod(gid, days))
			desc += t("report.joined", lang, "count", joined)
		}
		if showLeft {
			left = inScope(b.DB.GetDepartedMembersPeriod(gid, days))
			desc += t("report.left", lang, "count", left)
		}
		if showJoined && showLeft {
			desc += t("report.net", lang, "count", joined-left)
		}
		if g := b.DB.GetMemberGrowthStats(gid, days, tracked); g != nil {
			indicator := "➡️"
			if g.GrowthRate > 0 {
				indicator = "📈"
			} else if g.GrowthRate < 0 {
				indicator = "📉"
			}
			desc += t("report.growth_rate", lang, "indicator", indicator, "rate", math.Abs(g.GrowthRate))
		}
		desc += "\n"
	}

	health := b.DB.GetServerHealth(gid, tracked)
	if health != nil {
		if days <= 7 && health.WeeklyComparable {
			desc += t("report.trends_header_weekly", lang)
			desc += t("report.trends_posters", lang, "cur", health.Posters7d,
				"delta", formatHealthDelta(health.Posters7d, health.PostersPrev7d, lang))
			desc += t("report.trends_messages", lang, "cur", health.Messages7d,
				"delta", formatHealthDelta(health.Messages7d, health.MessagesPrev7d, lang))
			desc += "\n"
		} else if days >= 30 && health.MonthlyComparable {
			desc += t("report.trends_header_monthly", lang)
			for _, row := range []struct {
				key       string
				cur, prev int64
			}{
				{"report.trends_posters", health.Posters30d, health.PostersPrev30d},
				{"report.trends_messages", health.Messages30d, health.MessagesPrev30d},
				{"report.trends_joins", health.Joins30d, health.JoinsPrev30d},
				{"report.trends_leaves", health.Leaves30d, health.LeavesPrev30d},
				{"report.trends_returns", health.Returns30d, health.ReturnsPrev30d},
			} {
				desc += t(row.key, lang, "cur", row.cur, "delta", formatHealthDelta(row.cur, row.prev, lang))
			}
			desc += "\n"
		}
	}

	seg := b.DB.GetParticipationSegments(gid, days, tracked)
	haveSegments := seg.Lurkers != 0 || seg.Ghosts != 0
	var breadthMembers, breadthPosters int64
	if health != nil {
		breadthMembers = health.ActiveMembers
		breadthPosters = health.Posters30d
		if days <= 7 {
			breadthPosters = health.Posters7d
		}
	}
	if haveSegments || breadthMembers != 0 {
		desc += t("report.participation_header", lang)
		if breadthMembers != 0 {
			pct := float64(breadthPosters) / float64(breadthMembers) * 100
			desc += t("report.breadth", lang, "posters", breadthPosters, "members", breadthMembers, "pct", textfmt.Round(pct))
		}
		if haveSegments {
			desc += t("report.lurkers", lang, "count", seg.Lurkers, "pct", textfmt.Round(seg.LurkerPct))
			desc += t("report.ghosts", lang, "count", seg.Ghosts)
		}
		desc += "\n"
	}

	if contains(reportTypes, "retention") && days >= 30 {
		cohorts := b.DB.GetRetentionCohorts(gid, tracked)
		any := false
		for _, c := range cohorts {
			if c.TotalJoined > 0 {
				any = true
			}
		}
		if any {
			desc += t("report.retention_header", lang)
			for _, period := range db.CohortPeriods {
				c, ok := cohorts[period]
				if ok && c.TotalJoined > 0 {
					desc += t("report.retention_line", lang, "period", t("report.retention_period_"+period, lang),
						"rate", c.RetentionRate, "active", c.StillActive, "joined", c.TotalJoined)
				}
			}
			desc += "\n"
		}

		funnel := b.DB.GetActivationFunnel(gid, tracked)
		if len(funnel.Checkpoints) > 0 {
			desc += t("report.funnel_header", lang)
			for _, cp := range funnel.Checkpoints {
				desc += t("report.funnel_line", lang, "label", cp.Label, "rate", cp.Rate, "active", cp.Active, "matured", cp.Matured)
			}
			if d := funnel.LargestDropoff; d != nil {
				desc += t("report.funnel_dropoff", lang, "from_label", d[0], "to_label", d[1])
			}
			desc += "\n"
		}

		ls := b.DB.GetDepartureLifespan(gid, tracked)
		if ls.Sample >= 3 {
			desc += t("report.lifespan_header", lang)
			desc += t("report.lifespan_line", lang, "median", ls.MedianDays, "avg", ls.AvgDays, "pct", textfmt.Round(ls.EarlyChurnPct))
			desc += "\n"
		}
	}

	if len(topUsers) > 0 {
		desc += t("report.top_contributors_header", lang)
		for i, u := range topUsers {
			username := u.Username
			if username == "" {
				username = t("common.unknown", lang)
			}
			display := username
			if u.Nickname != nil && *u.Nickname != "" {
				display = *u.Nickname + " (" + username + ")"
			}
			desc += t("report.contributor_line", lang, "rank", i+1, "display", display, "count", u.TotalMessages)
		}
	} else {
		desc += t("report.top_contributors_header", lang) + t("report.no_activity", lang)
	}
	desc += t("report.more_info", lang)

	// Discord rejects descriptions over 4096 characters.
	if r := []rune(desc); len(r) > 4096 {
		desc = string(r[:4095]) + "…"
	}
	e.Description = desc
	return e
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// sendScheduledReport posts the report to channelID, rate limited per guild
// and retried on transient errors. Returns true when it was sent.
func (b *Bot) sendScheduledReport(guildID snowflake.ID, channelID int64, reportTypes []string, days int) bool {
	const maxRetries = 3
	l := reports.lock(guildID)
	l.Lock()
	defer l.Unlock()

	guildName := b.guildName(guildID)
	reports.mu.Lock()
	last, ok := reports.lastSend[guildID]
	reports.mu.Unlock()
	if ok {
		if since := time.Since(last); since < reportRateLimitWindow {
			wait := reportRateLimitWindow - since
			reportLog.Warn("Rate limit: waiting %.1fs before sending report to %s", wait.Seconds(), guildName)
			time.Sleep(wait)
		}
	}

	for attempt := 0; attempt < maxRetries; attempt++ {
		ch, found := b.Client.Caches.Channel(sf(channelID))
		if !found || ch.GuildID() != guildID || ch.Type() != discord.ChannelTypeGuildText {
			reportLog.Error("Report channel %d not found or not a text channel in guild %s", channelID, guildName)
			return false
		}
		e := b.generateActivityReport(guildID, guildName, days, reportTypes)
		_, err := b.Client.Rest.CreateMessage(ch.ID(), discord.MessageCreate{Embeds: []discord.Embed{e}})
		if err == nil {
			reports.mu.Lock()
			reports.lastSend[guildID] = time.Now()
			reports.mu.Unlock()
			reportLog.Info("Sent scheduled report to %s in guild %s", ch.Name(), guildName)
			return true
		}
		var rerr *rest.Error
		if errors.As(err, &rerr) && rerr.Response != nil {
			if rerr.Response.StatusCode == 403 {
				reportLog.Error("Missing permissions to send report in channel %d in guild %s", channelID, guildName)
				return false
			}
			reportLog.Error("Discord HTTP error sending report to %s: %v", guildName, err)
		} else {
			reportLog.Error("Failed to send scheduled report for guild %s (attempt %d/%d): %v", guildName, attempt+1, maxRetries, err)
		}
		if attempt < maxRetries-1 {
			time.Sleep(time.Duration(1<<attempt) * time.Second)
			continue
		}
		return false
	}
	reportLog.Error("Failed to send report to %s after %d attempts", guildName, maxRetries)
	return false
}
