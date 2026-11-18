// Package bot is the Discord side of LastSeen: the gateway client, event
// tracking, slash commands, admin panels, watches and scheduled reports.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo"
	disgobot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/disgo/sharding"
	"github.com/disgoorg/snowflake/v2"
	"github.com/gorilla/websocket"

	"lastseen/internal/config"
	"lastseen/internal/db"
	"lastseen/internal/logx"
)

var log = logx.Get("bot")

const (
	// ChunkConcurrency is how many guilds are chunked at once.
	ChunkConcurrency = 4
	// ChunkTimeout bounds one guild's member download.
	ChunkTimeout = 120 * time.Second
	// DBWorkers sizes the database connection pool.
	DBWorkers = 8
	// guildReadyTimeout: a shard counts as ready once no GUILD_CREATE has
	// arrived for this long.
	guildReadyTimeout = 2 * time.Second
	// IntentsRetryDelay: wait before exiting when Discord refuses the
	// privileged intents, so the restart loop retries slowly.
	IntentsRetryDelay = 600 * time.Second
)

// Version is the build's version ("v1.0.0-4-g9b79055"), shown in /about.
// main sets it from the version stamped in at build time.
var Version = "dev"

// Exit codes (start_bot.bat / systemd restart policy).
const (
	ExitOK     = 0
	ExitCrash  = 1
	ExitConfig = 2
)

// ExitError ends the bot with a process exit code.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }

// Bot holds the Discord client and the state shared by the bot's parts.
type Bot struct {
	Client   *disgobot.Client
	DB       *db.DB
	Cfg      *config.Config
	statuses *statusCache

	// Global opt-out lists and the guilds with at least one watch; checked
	// on hot event paths, so kept in memory.
	OptedOut    *idSet
	NoWatch     *idSet
	WatchGuilds *idSet

	startTime      atomic.Int64 // unix seconds of the first ready, 0 before
	commandsSynced atomic.Bool
	chunkStarted   atomic.Bool // the background chunker was launched once
	chunkRunning   atomic.Bool

	readyMu      sync.Mutex
	readyCycle   map[int]bool      // shards that finished READY in this cycle
	shardWaiters map[int]time.Time // shard -> last GUILD_CREATE seen while readying
	shardCount   int

	tracking *Tracking
	watch    *Watch
	ui       *uiRegistry
	cooldown *cooldowns

	commands      map[string]commandSpec
	autocompletes map[string]autocompleteFunc

	// readyCh is closed once the bot is first ready (every shard connected).
	readyCh   chan struct{}
	readyOnce sync.Once

	ctx    context.Context
	cancel context.CancelFunc
	fatal  chan *ExitError
}

// New creates the bot; nothing connects yet. Call Run to connect.
func New(cfg *config.Config, database *db.DB) *Bot {
	b := &Bot{
		DB:           database,
		Cfg:          cfg,
		statuses:     newStatusCache(),
		readyCycle:   map[int]bool{},
		shardWaiters: map[int]time.Time{},
		ui:           newUIRegistry(),
		cooldown:     newCooldowns(),
		fatal:        make(chan *ExitError, 1),
		readyCh:      make(chan struct{}),
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	b.OptedOut = newIDSet(database.GetOptedOutUserIDs())
	b.NoWatch = newIDSet(database.GetNoWatchUserIDs())
	b.WatchGuilds = newIDSet(database.GetWatchGuildIDs())
	return b
}

// fail ends Run with the given exit code (first call wins).
func (b *Bot) fail(code int, err error) {
	select {
	case b.fatal <- &ExitError{Code: code, Err: err}:
	default:
	}
}

// goSafe runs fn in a goroutine, logging a panic instead of crashing the bot.
func goSafe(what string, fn func()) {
	go func() {
		defer recoverLog(what)
		fn()
	}()
}

func recoverLog(what string) {
	if r := recover(); r != nil {
		log.Error("Error in %s: %v\n%s", what, r, debug.Stack())
	}
}

// Run connects to Discord and blocks until ctx is cancelled (clean exit) or
// a fatal error occurs.
func (b *Bot) Run(ctx context.Context) error {
	b.initComponents()

	log.Info("Starting bot...")
	// The shard count must be known before any shard connects (ready
	// tracking waits for every shard), so ask Discord up front when it is
	// not configured.
	b.shardCount = b.Cfg.ShardCount
	if b.shardCount <= 0 {
		gb, err := rest.New(rest.NewClient(b.Cfg.BotToken)).GetGatewayBot()
		if err != nil {
			return b.startupError(err)
		}
		b.shardCount = max(gb.Shards, 1)
	}
	ids := make([]int, b.shardCount)
	for i := range ids {
		ids[i] = i
	}
	shardOpts := []sharding.ConfigOpt{
		sharding.WithShardCount(b.shardCount),
		sharding.WithShardIDs(ids...),
		sharding.WithAutoScaling(false),
		sharding.WithGatewayConfigOpts(
			gateway.WithIntents(gateway.IntentGuilds, gateway.IntentGuildMembers,
				gateway.IntentGuildPresences, gateway.IntentGuildMessages),
			gateway.WithLargeThreshold(250),
			gateway.WithAutoReconnect(false),
		),
		sharding.WithCloseHandler(b.onShardClosed),
	}
	libLog := slog.New(logx.NewSlogHandler("disgo", slog.LevelInfo))
	client, err := disgo.New(b.Cfg.BotToken,
		disgobot.WithLogger(libLog),
		disgobot.WithShardManagerConfigOpts(shardOpts...),
		disgobot.WithCacheConfigOpts(
			cache.WithCaches(cache.FlagGuilds, cache.FlagMembers, cache.FlagRoles, cache.FlagChannels, cache.FlagPresences),
			cache.WithPresenceCache(cache.NewPresenceCache(b.statuses)),
		),
		disgobot.WithEventListeners(b.listeners()),
	)
	if err != nil {
		return b.startupError(err)
	}
	b.Client = client
	client.MemberChunkingManager = &presenceChunker{MemberChunkingManager: client.MemberChunkingManager, statuses: b.statuses}

	if err := b.openShards(ids); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return nil
	case e := <-b.fatal:
		return e
	}
}

// startupError maps a REST failure before connecting: a 401 means the token
// is wrong (configuration error), anything else is a crash.
func (b *Bot) startupError(err error) error {
	var rerr *rest.Error
	if errors.As(err, &rerr) && rerr.Response != nil && rerr.Response.StatusCode == 401 {
		log.Error("Configuration error: Discord rejected the bot token.")
		log.Error("%v", err)
		log.Error("Check DISCORD_BOT_TOKEN in the .env file, then restart the bot.")
		return &ExitError{Code: ExitConfig, Err: err}
	}
	return &ExitError{Code: ExitCrash, Err: err}
}

// openShards connects every shard (the identify rate limiter paces them),
// handling the fatal close code of the first one that fails.
func (b *Bot) openShards(ids []int) error {
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = b.Client.ShardManager.OpenShard(b.ctx, id)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return b.fatalClose(err, ids[i])
		}
	}
	return nil
}

// onShardClosed runs when a shard's connection ends. disgo's own
// auto-reconnect is off so the bot sees every disconnect; it reconnects
// the shard itself.
func (b *Bot) onShardClosed(g gateway.Gateway, err error, reconnect bool) {
	shardID := g.ShardID()
	if b.ctx.Err() != nil {
		return // shutting down
	}
	if b.tracking != nil {
		b.tracking.onShardDisconnect(shardID)
	}
	if !reconnect {
		if e := b.fatalClose(err, shardID); e != nil {
			var ee *ExitError
			if errors.As(e, &ee) {
				b.fail(ee.Code, ee.Err)
			}
		}
		return
	}
	go func() {
		if err := g.Open(b.ctx); err != nil && b.ctx.Err() == nil {
			if e := b.fatalClose(err, shardID); e != nil {
				var ee *ExitError
				if errors.As(e, &ee) {
					b.fail(ee.Code, ee.Err)
				}
			}
		}
	}()
}

// fatalClose maps a fatal gateway close to an exit code: a bad
// token is a configuration error, refused privileged intents wait
// IntentsRetryDelay and then crash (so the restart loop retries slowly).
func (b *Bot) fatalClose(err error, shardID int) error {
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		switch gateway.CloseEventCodeByCode(ce.Code) {
		case gateway.CloseEventCodeAuthenticationFailed:
			log.Error("Configuration error: Discord rejected the bot token.")
			log.Error("%v", err)
			log.Error("Check DISCORD_BOT_TOKEN in the .env file, then restart the bot.")
			return &ExitError{Code: ExitConfig, Err: err}
		case gateway.CloseEventCodeDisallowedIntent:
			log.Error("Discord refused the privileged intents (Presences / Server Members). " +
				"Check the Developer Portal and the intent review status.")
			log.Error("Retrying in %d minutes...", int(IntentsRetryDelay/time.Minute))
			select {
			case <-time.After(IntentsRetryDelay):
			case <-b.ctx.Done():
			}
			return &ExitError{Code: ExitCrash, Err: fmt.Errorf("privileged intents required: %w", err)}
		}
	}
	return &ExitError{Code: ExitCrash, Err: fmt.Errorf("shard %d closed: %w", shardID, err)}
}

// Close shuts down: final flush, disconnect, then the database.
func (b *Bot) Close() {
	if b.tracking != nil {
		b.tracking.unload()
	}
	b.cancel()
	if b.Client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		b.Client.Close(ctx)
		cancel()
	}
	log.Info("Bot is shutting down, closing database connection pool...")
	if err := b.DB.Close(); err != nil {
		log.Error("Error closing database pool: %v", err)
	}
}

// initComponents creates the tracking and watch components and registers the
// slash commands, before any shard connects.
func (b *Bot) initComponents() {
	b.tracking = newTracking(b)
	b.watch = newWatch(b)
	b.registerCommands()
}

// --- guilds and members from the cache ---

// guildIDs is every guild the bot is in (available or not), like bot.guilds.
func (b *Bot) guildIDs() []snowflake.ID {
	seen := map[snowflake.ID]bool{}
	var out []snowflake.ID
	add := func(id snowflake.ID) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for g := range b.Client.Caches.Guilds() {
		add(g.ID)
	}
	for _, id := range b.Client.Caches.UnavailableGuildIDs() {
		add(id)
	}
	for _, id := range b.Client.Caches.UnreadyGuildIDs() {
		add(id)
	}
	return out
}

func (b *Bot) guildName(id snowflake.ID) string {
	if g, ok := b.Client.Caches.Guild(id); ok {
		return g.Name
	}
	return id.String()
}

// shardOf is the shard a guild belongs to.
func (b *Bot) shardOf(guildID snowflake.ID) int {
	if b.shardCount <= 1 {
		return 0
	}
	return int((uint64(guildID) >> 22) % uint64(b.shardCount))
}

// isChunked reports whether every member of the guild is cached.
func (b *Bot) isChunked(guildID snowflake.ID) bool {
	g, ok := b.Client.Caches.Guild(guildID)
	if !ok {
		return false
	}
	return b.Client.Caches.MembersLen(guildID) >= g.MemberCount
}

// isOnline is member.status != offline.
func (b *Bot) isOnline(guildID, userID snowflake.ID) bool { return b.statuses.Online(guildID, userID) }

// chunkGuild downloads a guild's full member list (with presences), bounded
// by ChunkTimeout. Returns the number of members received.
func (b *Bot) chunkGuild(guildID snowflake.ID) (int, error) {
	ch, cancel, err := b.Client.MemberChunkingManager.RequestAllMembersChan(guildID)
	if err != nil {
		return 0, err
	}
	timer := time.NewTimer(ChunkTimeout)
	defer timer.Stop()
	n := 0
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return n, nil
			}
			n++
		case <-timer.C:
			cancel()
			// Drain so the gateway goroutine is never left blocked on send.
			go func() {
				for range ch {
				}
			}()
			return n, context.DeadlineExceeded
		case <-b.ctx.Done():
			cancel()
			go func() {
				for range ch {
				}
			}()
			return n, b.ctx.Err()
		}
	}
}

// presenceChunker applies the presences in member chunks to the status
// cache (disgo only caches the members), so chunked members are not all
// treated as offline.
type presenceChunker struct {
	disgobot.MemberChunkingManager
	statuses *statusCache
}

func (p *presenceChunker) HandleChunk(payload gateway.EventGuildMembersChunk) {
	for _, pr := range payload.Presences {
		p.statuses.Put(payload.GuildID, pr.PresenceUser.ID, pr)
	}
	p.MemberChunkingManager.HandleChunk(payload)
}

// --- event wiring ---

func (b *Bot) listeners() *events.ListenerAdapter {
	return &events.ListenerAdapter{
		OnReady:            b.onReadyEvent,
		OnResumed:          b.onResumed,
		OnGuildReady:       b.onGuildReady,
		OnGuildAvailable:   func(e *events.GuildAvailable) { b.onGuildAvailable(e.GuildID) },
		OnGuildUnavailable: func(e *events.GuildUnavailable) { b.tracking.onGuildUnavailable(e.GuildID) },
		OnGuildJoin:        func(e *events.GuildJoin) { b.tracking.onGuildJoin(e.GuildID, e.Guild.Name) },
		OnGuildLeave:       func(e *events.GuildLeave) { b.tracking.onGuildRemove(e.GuildID, e.Guild.Name) },
		OnGuildUpdate: func(e *events.GuildUpdate) {
			b.tracking.onGuildUpdate(e.GuildID, e.OldGuild.Name, e.Guild.Name)
		},
		OnGuildMemberJoin: func(e *events.GuildMemberJoin) { b.tracking.onMemberJoin(e.GuildID, e.Member) },
		OnGuildMemberLeave: func(e *events.GuildMemberLeave) {
			b.tracking.onMemberRemove(e.GuildID, e.User, e.Member)
			if e.Member.User.ID != 0 { // as in tracking: only cached members count as departures
				goSafe("watch member remove", func() { b.watch.onMemberRemove(e.GuildID, e.User) })
			}
		},
		OnGuildMemberUpdate: func(e *events.GuildMemberUpdate) { b.tracking.onMemberUpdate(e.GuildID, e.OldMember, e.Member) },
		OnGuildMessageCreate: func(e *events.GuildMessageCreate) {
			b.tracking.onMessage(e.GuildID, e.Message)
		},
		OnPresenceUpdate: func(e *events.PresenceUpdate) { b.tracking.onPresenceUpdate(e) },

		OnApplicationCommandInteraction: func(e *events.ApplicationCommandInteractionCreate) {
			b.checkInteractionAge(e.ApplicationCommandInteraction, e.Data.CommandName())
			goSafe("command", func() { b.handleCommand(e) })
		},
		OnAutocompleteInteraction: func(e *events.AutocompleteInteractionCreate) {
			b.checkInteractionAge(e.AutocompleteInteraction, e.Data.CommandName)
			goSafe("autocomplete", func() { b.handleAutocomplete(e) })
		},
		OnComponentInteraction: func(e *events.ComponentInteractionCreate) {
			b.checkInteractionAge(e.ComponentInteraction, e.Data.CustomID())
			goSafe("component", func() { b.handleComponent(e) })
		},
		OnModalSubmit: func(e *events.ModalSubmitInteractionCreate) {
			b.checkInteractionAge(e.ModalSubmitInteraction, e.Data.CustomID)
			goSafe("modal", func() { b.handleModal(e) })
		},
	}
}

// checkInteractionAge flags interactions that arrive with most of their 3s
// acknowledgement window already gone.
func (b *Bot) checkInteractionAge(i discord.Interaction, name string) {
	age := time.Since(i.CreatedAt()).Seconds()
	if age > 2.0 {
		if name == "" {
			name = "?"
		}
		gid := "None"
		if g := i.GuildID(); g != nil {
			gid = g.String()
		}
		log.Warn("Interaction %s '%s' was already %.2fs old at dispatch (guild %s)",
			interactionTypeName(i.Type()), name, age, gid)
	}
}

func interactionTypeName(t discord.InteractionType) string {
	switch t {
	case discord.InteractionTypeApplicationCommand:
		return "application_command"
	case discord.InteractionTypeComponent:
		return "component"
	case discord.InteractionTypeAutocomplete:
		return "autocomplete"
	case discord.InteractionTypeModalSubmit:
		return "modal_submit"
	}
	return "ping"
}

// --- ready handling: per shard, and once every shard is ready ---

// onReadyEvent: a shard (re)identified. Its guilds will now be re-sent, so
// their cached members and statuses are dropped and rebuilt from scratch.
func (b *Bot) onReadyEvent(e *events.Ready) {
	shardID := e.ShardID()
	for _, g := range e.Guilds {
		b.Client.Caches.MemberCache().GroupRemove(g.ID)
		b.statuses.GroupRemove(g.ID)
	}
	b.readyMu.Lock()
	b.shardWaiters[shardID] = time.Now()
	b.readyMu.Unlock()
	go b.waitShardReady(shardID)
}

func (b *Bot) onGuildReady(e *events.GuildReady) {
	shardID := e.ShardID()
	b.readyMu.Lock()
	if _, ok := b.shardWaiters[shardID]; ok {
		b.shardWaiters[shardID] = time.Now()
	}
	b.readyMu.Unlock()
	// After the first on_ready, a guild arriving in a shard's re-identify is
	// "available" again: its cache must be re-chunked and reconciled.
	if b.chunkStarted.Load() {
		b.onGuildAvailable(e.GuildID)
	}
}

// waitShardReady waits until the shard's guilds have stopped arriving, then
// fires shard ready, and on_ready once every shard is ready.
func (b *Bot) waitShardReady(shardID int) {
	for {
		time.Sleep(200 * time.Millisecond)
		b.readyMu.Lock()
		last, ok := b.shardWaiters[shardID]
		if !ok {
			b.readyMu.Unlock()
			return
		}
		pending := false
		for _, id := range b.Client.Caches.UnreadyGuildIDs() {
			if b.shardOf(id) == shardID {
				pending = true
				break
			}
		}
		if pending && time.Since(last) < guildReadyTimeout {
			b.readyMu.Unlock()
			continue
		}
		delete(b.shardWaiters, shardID)
		b.readyCycle[shardID] = true
		all := len(b.readyCycle) >= b.shardCount
		if all {
			b.readyCycle = map[int]bool{}
		}
		b.readyMu.Unlock()

		log.Info("Shard %d is ready", shardID)
		b.tracking.onShardReady(shardID)
		if all {
			b.onReady()
		}
		return
	}
}

func (b *Bot) onResumed(e *events.Resumed) { b.tracking.onShardResumed(e.ShardID()) }

// onReady runs each time every shard is ready: at startup and after a
// full reconnect.
func (b *Bot) onReady() {
	if b.startTime.Load() == 0 {
		b.startTime.Store(time.Now().Unix())
	}
	b.readyOnce.Do(func() { close(b.readyCh) })
	self, _ := b.Client.Caches.SelfUser()
	log.Info("Logged in as %s (ID: %s)", userTag(self.User), self.ID)
	log.Info("Connected to %d guilds across %d shard(s)", len(b.guildIDs()), b.shardCount)

	if !b.chunkRunning.Load() {
		b.chunkStarted.Store(true)
		b.chunkRunning.Store(true)
		goSafe("chunker", func() {
			defer b.chunkRunning.Store(false)
			b.chunkGuildsBackground()
		})
	}

	if !b.commandsSynced.Load() {
		if n, err := b.syncCommands(); err != nil {
			log.Error("Failed to sync commands: %v", err)
		} else {
			b.commandsSynced.Store(true)
			log.Info("Synced %d command(s) with Discord", n)
		}
	}
	log.Info("Bot is ready!")

	goSafe("tracking on_ready", b.tracking.onReady)
	b.watch.onReady()
}

// chunkGuildsBackground fills every guild's member cache, announcing each
// complete one so tracking can reconcile it. A failed or timed-out chunk is
// never announced (a partial cache would make real members look departed).
//
// Guilds Discord sent no data for (unavailable) are skipped: Discord doesn't
// answer member requests for them, so asking would only time out.
// onGuildAvailable chunks them when they arrive.
func (b *Bot) chunkGuildsBackground() {
	var toChunk, unavailable []snowflake.ID
	for _, id := range b.guildIDs() {
		_, cached := b.Client.Caches.Guild(id)
		switch {
		case !cached:
			unavailable = append(unavailable, id)
		case b.isChunked(id):
			b.tracking.onGuildChunked(id)
		default:
			toChunk = append(toChunk, id)
		}
	}
	for _, id := range unavailable {
		log.Warn("Guild %s is unavailable (Discord sent no data for it); it will be chunked when it becomes available", id)
	}
	if len(toChunk) == 0 {
		if len(unavailable) == 0 {
			log.Info("All guild member caches already complete, no chunking needed")
		}
		return
	}
	log.Info("Chunking %d guild(s) in background...", len(toChunk))
	sem := make(chan struct{}, ChunkConcurrency)
	var wg sync.WaitGroup
	var failed atomic.Int64
	for _, id := range toChunk {
		wg.Add(1)
		sem <- struct{}{}
		go func(id snowflake.ID) {
			defer wg.Done()
			defer func() { <-sem }()
			name := b.guildName(id)
			if _, err := b.chunkGuild(id); err != nil {
				failed.Add(1)
				if errors.Is(err, context.DeadlineExceeded) {
					log.Error("  ✗ Timed out chunking %s after %ds", name, int(ChunkTimeout.Seconds()))
				} else {
					log.Error("  ✗ Failed to chunk %s: %v", name, err)
				}
				return
			}
			total := 0
			if g, ok := b.Client.Caches.Guild(id); ok {
				total = g.MemberCount
			}
			log.Info("  ✓ %s: %d/%d members cached, %d online", name,
				b.Client.Caches.MembersLen(id), total, b.statuses.GroupLen(id))
			b.tracking.onGuildChunked(id)
		}(id)
	}
	wg.Wait()
	summary := fmt.Sprintf("Background chunking finished: %d/%d guild(s) chunked", len(toChunk)-int(failed.Load()), len(toChunk))
	if len(unavailable) > 0 {
		summary += fmt.Sprintf(", %d unavailable", len(unavailable))
	}
	log.Info("%s", summary)
}

// onGuildAvailable chunks a guild that came back after startup (outage
// recovery), then announces it for reconciliation.
func (b *Bot) onGuildAvailable(guildID snowflake.ID) {
	if !b.chunkStarted.Load() {
		return
	}
	goSafe("guild available", func() {
		name := b.guildName(guildID)
		if !b.isChunked(guildID) {
			log.Info("Guild %s became available, chunking...", name)
			if _, err := b.chunkGuild(guildID); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					log.Error("  ✗ Timed out chunking %s after %ds", name, int(ChunkTimeout.Seconds()))
				} else {
					log.Error("  ✗ Failed to chunk %s: %v", name, err)
				}
				return
			}
			total := 0
			if g, ok := b.Client.Caches.Guild(guildID); ok {
				total = g.MemberCount
			}
			log.Info("  ✓ %s: %d/%d members cached", name, b.Client.Caches.MembersLen(guildID), total)
		}
		b.tracking.onGuildChunked(guildID)
	})
}
