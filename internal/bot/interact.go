package bot

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"

	"lastseen/internal/db"
	"lastseen/internal/logx"
)

// A small interaction framework: views (buttons and select menus with
// automatic row layout, owner checks, refreshed timeouts, stop()), modals,
// per-command cooldowns and the global command error reply.

var ilog = logx.Get("interactions")

// Msg is a message to send, or an edit to apply to one.
type Msg struct {
	Content      string
	ClearContent bool // edits: remove the content
	Embeds       []discord.Embed
	View         *View // attach (and start) this view
	ClearView    bool  // edits: remove all components
	Files        []*discord.File
	Ephemeral    bool
}

func embedMsg(e discord.Embed, ephemeral bool) Msg {
	return Msg{Embeds: []discord.Embed{e}, Ephemeral: ephemeral}
}

func (m Msg) components() []discord.LayoutComponent {
	if m.View != nil {
		return m.View.components()
	}
	return nil
}

func (m Msg) create() discord.MessageCreate {
	mc := discord.MessageCreate{Content: m.Content, Embeds: m.Embeds, Files: m.Files, Components: m.components()}
	if m.Ephemeral {
		mc.Flags = discord.MessageFlagEphemeral
	}
	return mc
}

func (m Msg) update() discord.MessageUpdate {
	var mu discord.MessageUpdate
	if m.Content != "" || m.ClearContent {
		c := m.Content
		mu.Content = &c
	}
	if m.Embeds != nil {
		e := m.Embeds
		mu.Embeds = &e
	}
	if m.View != nil {
		c := m.View.components()
		mu.Components = &c
	} else if m.ClearView {
		c := []discord.LayoutComponent{}
		mu.Components = &c
	}
	mu.Files = m.Files
	return mu
}

// Ix is one interaction being handled.
type Ix struct {
	b         *Bot
	i         discord.Interaction
	acked     atomic.Bool
	name      string // command or custom id, for logs
	GuildID   snowflake.ID
	ChannelID snowflake.ID
	User      discord.User
	Member    *discord.ResolvedMember

	cmd    *discord.SlashCommandInteractionData
	values []string // select menu values
	modal  *discord.ModalSubmitInteractionData
}

func (b *Bot) newIx(i discord.Interaction, name string) *Ix {
	ix := &Ix{b: b, i: i, name: name, User: i.User(), Member: i.Member()}
	if g := i.GuildID(); g != nil {
		ix.GuildID = *g
	}
	ix.ChannelID = i.Channel().ID()
	return ix
}

// IsDone is interaction.response.is_done().
func (ix *Ix) IsDone() bool { return ix.acked.Load() }

func (ix *Ix) respond(t discord.InteractionResponseType, data discord.InteractionResponseData) error {
	ix.acked.Store(true)
	err := ix.b.Client.Rest.CreateInteractionResponse(ix.i.ID(), ix.i.Token(), discord.InteractionResponse{Type: t, Data: data})
	if err != nil {
		ilog.Warn("Failed to respond to interaction '%s': %v", ix.name, err)
	}
	return err
}

// Respond is interaction.response.send_message.
func (ix *Ix) Respond(m Msg) error {
	if m.View != nil {
		m.View.start()
	}
	return ix.respond(discord.InteractionResponseTypeCreateMessage, m.create())
}

// Defer is interaction.response.defer (thinking=True for commands).
func (ix *Ix) Defer(ephemeral bool) error {
	var data discord.MessageCreate
	if ephemeral {
		data.Flags = discord.MessageFlagEphemeral
	}
	return ix.respond(discord.InteractionResponseTypeDeferredCreateMessage, data)
}

// DeferUpdate acknowledges a component interaction without a new message.
func (ix *Ix) DeferUpdate() error {
	return ix.respond(discord.InteractionResponseTypeDeferredUpdateMessage, nil)
}

// Update is interaction.response.edit_message (component interactions).
func (ix *Ix) Update(m Msg) error {
	if m.View != nil {
		m.View.start()
	}
	return ix.respond(discord.InteractionResponseTypeUpdateMessage, m.update())
}

// SendModal is interaction.response.send_modal.
func (ix *Ix) SendModal(md *Modal) error {
	md.start()
	return ix.respond(discord.InteractionResponseTypeModal, md.create())
}

// Followup is interaction.followup.send.
func (ix *Ix) Followup(m Msg) (*discord.Message, error) {
	if m.View != nil {
		m.View.start()
	}
	msg, err := ix.b.Client.Rest.CreateFollowupMessage(ix.i.ApplicationID(), ix.i.Token(), m.create())
	if err != nil {
		ilog.Warn("Failed to send followup for '%s': %v", ix.name, err)
	}
	return msg, err
}

// EditOriginal is interaction.edit_original_response.
func (ix *Ix) EditOriginal(m Msg) error {
	if m.View != nil {
		m.View.start()
	}
	_, err := ix.b.Client.Rest.UpdateInteractionResponse(ix.i.ApplicationID(), ix.i.Token(), m.update())
	if err != nil {
		ilog.Debug("Failed to edit original response for '%s': %v", ix.name, err)
	}
	return err
}

// Send replies with a fresh message: the response when not yet answered,
// else a followup (used for errors after a defer).
func (ix *Ix) Send(m Msg) error {
	if ix.IsDone() {
		_, err := ix.Followup(m)
		return err
	}
	return ix.Respond(m)
}

// Autocomplete answers an autocomplete interaction.
func (ix *Ix) Autocomplete(choices []discord.AutocompleteChoice) error {
	if choices == nil {
		choices = []discord.AutocompleteChoice{}
	}
	return ix.respond(discord.InteractionResponseTypeAutocompleteResult, discord.AutocompleteResult{Choices: choices})
}

// Values are a select menu's chosen values.
func (ix *Ix) Values() []string { return ix.values }

// Text is a modal text input's value.
func (ix *Ix) Text(field string) string {
	if ix.modal == nil {
		return ""
	}
	return ix.modal.Text(field)
}

// GuildName is the interaction guild's cached name.
func (ix *Ix) GuildName() string { return ix.b.guildName(ix.GuildID) }

// memberCtx describes the invoking member for the permission helpers.
func (ix *Ix) memberCtx() memberCtx {
	m := memberCtx{GuildID: ix.GuildID}
	if ix.Member != nil {
		m.Permissions = ix.Member.Permissions
		m.Roles = ix.b.memberRoles(ix.GuildID, ix.Member.RoleIDs)
	}
	return m
}

// channelParentID is the parent of a thread channel (nil otherwise).
func (ix *Ix) channelParentID() *snowflake.ID {
	if ch, ok := ix.b.Client.Caches.Channel(ix.ChannelID); ok {
		if th, ok := ch.(discord.GuildThread); ok {
			return th.ParentID()
		}
	}
	if th, ok := ix.i.Channel().MessageChannel.(discord.GuildThread); ok {
		return th.ParentID()
	}
	return nil
}

// ==================== Views ====================

const (
	widthButton = 1
	widthSelect = 5
)

// Item is a button or select menu of a View.
type Item struct {
	view        *View
	index       int
	kind        discord.ComponentType
	Label       string
	Emoji       string
	Style       discord.ButtonStyle
	Disabled    bool
	Row         int // -1 = automatic placement
	URL         string
	Placeholder string
	Options     []discord.StringSelectMenuOption
	MinValues   int
	MaxValues   int
	Hidden      bool // removed from the view (remove_item)
	handler     func(ix *Ix)
}

func (it *Item) width() int {
	if it.kind == discord.ComponentTypeButton {
		return widthButton
	}
	return widthSelect
}

func (it *Item) customID() string { return "v:" + it.view.id + ":" + strconv.Itoa(it.index) }

func (it *Item) component() discord.InteractiveComponent {
	switch it.kind {
	case discord.ComponentTypeButton:
		btn := discord.ButtonComponent{Style: it.Style, Label: it.Label, Disabled: it.Disabled}
		if it.URL != "" {
			btn.URL = it.URL
		} else {
			btn.CustomID = it.customID()
		}
		if it.Emoji != "" {
			e := discord.ComponentEmoji{Name: it.Emoji}
			btn.Emoji = &e
		}
		return btn
	default:
		minV := it.MinValues
		return discord.StringSelectMenuComponent{
			CustomID:    it.customID(),
			Placeholder: it.Placeholder,
			MinValues:   &minV,
			MaxValues:   it.MaxValues,
			Options:     it.Options,
			Disabled:    it.Disabled,
		}
	}
}

// View is discord.ui.View: interactive components bound to handlers.
type View struct {
	b         *Bot
	id        string
	timeout   time.Duration
	mu        sync.Mutex // serialises item callbacks
	itemsMu   sync.Mutex
	items     []*Item
	check     func(ix *Ix) bool
	onTimeout func()
	timer     *time.Timer
	started   atomic.Bool
	stopped   atomic.Bool
}

func randomID() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func (b *Bot) newView(timeout time.Duration) *View {
	return &View{b: b, id: randomID(), timeout: timeout}
}

// ownerOnly limits the view to its owner (OwnerOnlyView).
func (v *View) ownerOnly(ownerID snowflake.ID, lang string) *View {
	v.check = func(ix *Ix) bool {
		if ix.User.ID == ownerID {
			return true
		}
		ix.Respond(Msg{Content: t("errors.not_your_panel", lang), Ephemeral: true})
		return false
	}
	return v
}

func (v *View) add(it *Item) *Item {
	v.itemsMu.Lock()
	defer v.itemsMu.Unlock()
	it.view = v
	it.index = len(v.items)
	v.items = append(v.items, it)
	return it
}

// Button adds a button (row -1 = automatic).
func (v *View) Button(label string, style discord.ButtonStyle, emoji string, row int, fn func(ix *Ix)) *Item {
	return v.add(&Item{kind: discord.ComponentTypeButton, Label: label, Style: style, Emoji: emoji, Row: row, handler: fn})
}

// Select adds a string select menu.
func (v *View) Select(placeholder string, options []discord.StringSelectMenuOption, minV, maxV, row int, fn func(ix *Ix)) *Item {
	return v.add(&Item{kind: discord.ComponentTypeStringSelectMenu, Placeholder: placeholder, Options: options,
		MinValues: minV, MaxValues: maxV, Row: row, handler: fn})
}

// components lays the items out in rows: items with an explicit row
// first, then each other item in the first row with room.
func (v *View) components() []discord.LayoutComponent {
	v.itemsMu.Lock()
	defer v.itemsMu.Unlock()
	var visible []*Item
	for _, it := range v.items {
		if !it.Hidden {
			visible = append(visible, it)
		}
	}
	order := append([]*Item(nil), visible...)
	sort.SliceStable(order, func(i, j int) bool {
		ri, rj := order[i].Row, order[j].Row
		if ri < 0 {
			ri = 1 << 30
		}
		if rj < 0 {
			rj = 1 << 30
		}
		return ri < rj
	})
	var weights [5]int
	rendered := map[*Item]int{}
	for _, it := range order {
		if it.Row >= 0 && it.Row < 5 {
			weights[it.Row] += it.width()
			rendered[it] = it.Row
			continue
		}
		for r := 0; r < 5; r++ {
			if weights[r]+it.width() <= 5 {
				weights[r] += it.width()
				rendered[it] = r
				break
			}
		}
	}
	sort.SliceStable(visible, func(i, j int) bool { return rendered[visible[i]] < rendered[visible[j]] })
	var rows []discord.LayoutComponent
	for i := 0; i < len(visible); {
		r := rendered[visible[i]]
		var comps []discord.InteractiveComponent
		for ; i < len(visible) && rendered[visible[i]] == r; i++ {
			comps = append(comps, visible[i].component())
		}
		rows = append(rows, discord.ActionRowComponent{Components: comps})
	}
	return rows
}

// start registers the view so its components are listened to.
func (v *View) start() {
	if v.stopped.Load() || !v.started.CompareAndSwap(false, true) {
		return
	}
	v.b.ui.addView(v)
	if v.timeout > 0 {
		v.timer = time.AfterFunc(v.timeout, v.expire)
	}
}

func (v *View) refreshTimeout() {
	if v.timer != nil {
		v.timer.Reset(v.timeout)
	}
}

func (v *View) expire() {
	if v.stopped.Swap(true) {
		return
	}
	v.b.ui.removeView(v.id)
	if v.onTimeout != nil {
		goSafe("view timeout", v.onTimeout)
	}
}

// Stop is View.stop(): no further interactions are handled.
func (v *View) Stop() {
	if v.stopped.Swap(true) {
		return
	}
	if v.timer != nil {
		v.timer.Stop()
	}
	v.b.ui.removeView(v.id)
}

func (v *View) dispatch(ix *Ix, index int) {
	v.itemsMu.Lock()
	if index < 0 || index >= len(v.items) {
		v.itemsMu.Unlock()
		return
	}
	it := v.items[index]
	v.itemsMu.Unlock()
	if it.handler == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped.Load() {
		return
	}
	if v.check != nil && !v.check(ix) {
		return
	}
	v.refreshTimeout()
	it.handler(ix)
}

// ==================== Modals ====================

// modalGC bounds how long an unsubmitted modal is remembered.
const modalGC = time.Hour

// Modal is discord.ui.Modal.
type Modal struct {
	b        *Bot
	id       string
	Title    string
	comps    []discord.LayoutComponent
	fields   int
	onSubmit func(ix *Ix)
	timer    *time.Timer
}

func (b *Bot) newModal(title string, onSubmit func(ix *Ix)) *Modal {
	return &Modal{b: b, id: randomID(), Title: title, onSubmit: onSubmit}
}

// TextDisplay adds a text block.
func (md *Modal) TextDisplay(content string) {
	md.comps = append(md.comps, discord.TextDisplayComponent{Content: content})
}

// TextInput describes a modal text field.
type TextInput struct {
	Label, Description, Placeholder, Default string
	Paragraph, Required                      bool
	MaxLength                                int
}

// Input adds a labelled text input and returns its field key.
func (md *Modal) Input(in TextInput) string {
	key := "f" + strconv.Itoa(md.fields)
	md.fields++
	style := discord.TextInputStyleShort
	if in.Paragraph {
		style = discord.TextInputStyleParagraph
	}
	ti := discord.TextInputComponent{CustomID: key, Style: style, MaxLength: in.MaxLength,
		Required: in.Required, Placeholder: in.Placeholder, Value: in.Default}
	md.comps = append(md.comps, discord.LabelComponent{Label: in.Label, Description: in.Description, Component: ti})
	return key
}

func (md *Modal) create() discord.ModalCreate {
	return discord.ModalCreate{CustomID: "m:" + md.id, Title: md.Title, Components: md.comps}
}

func (md *Modal) start() {
	md.b.ui.addModal(md)
	md.timer = time.AfterFunc(modalGC, func() { md.b.ui.removeModal(md.id) })
}

// ==================== Registry ====================

type uiRegistry struct {
	mu     sync.Mutex
	views  map[string]*View
	modals map[string]*Modal
}

func newUIRegistry() *uiRegistry {
	return &uiRegistry{views: map[string]*View{}, modals: map[string]*Modal{}}
}

func (r *uiRegistry) addView(v *View) {
	r.mu.Lock()
	r.views[v.id] = v
	r.mu.Unlock()
}

func (r *uiRegistry) removeView(id string) {
	r.mu.Lock()
	delete(r.views, id)
	r.mu.Unlock()
}

func (r *uiRegistry) view(id string) *View {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.views[id]
}

func (r *uiRegistry) addModal(m *Modal) {
	r.mu.Lock()
	r.modals[m.id] = m
	r.mu.Unlock()
}

func (r *uiRegistry) removeModal(id string) {
	r.mu.Lock()
	delete(r.modals, id)
	r.mu.Unlock()
}

// takeModal returns and forgets a modal (a modal is stopped once submitted).
func (r *uiRegistry) takeModal(id string) *Modal {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.modals[id]
	delete(r.modals, id)
	return m
}

// ==================== Cooldowns ====================

type cooldownKey struct {
	cmd         string
	guild, user snowflake.ID
}

// cooldowns is app_commands.checks.cooldown(1, per): one use per window.
type cooldowns struct {
	mu      sync.Mutex
	windows map[cooldownKey]time.Time
}

func newCooldowns() *cooldowns { return &cooldowns{windows: map[cooldownKey]time.Time{}} }

// hit consumes the cooldown; returns the remaining wait when already used.
func (c *cooldowns) hit(k cooldownKey, per time.Duration) (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if len(c.windows) > 10000 {
		for key, w := range c.windows {
			if now.Sub(w) > 2*time.Minute {
				delete(c.windows, key)
			}
		}
	}
	if w, ok := c.windows[k]; ok && now.Sub(w) < per {
		return per - now.Sub(w), true
	}
	c.windows[k] = now
	return 0, false
}

// ==================== Dispatch ====================

type commandSpec struct {
	handler  func(ix *Ix)
	cooldown time.Duration
	perGuild bool // cooldown keyed by (guild, user) instead of user
}

// commandPath is "name" or "group sub".
func commandPath(d discord.SlashCommandInteractionData) string {
	p := d.CommandName()
	if g := d.SubCommandGroupName; g != nil {
		p += " " + *g
	}
	if s := d.SubCommandName; s != nil {
		p += " " + *s
	}
	return p
}

func (b *Bot) handleCommand(e *events.ApplicationCommandInteractionCreate) {
	data, ok := e.Data.(discord.SlashCommandInteractionData)
	if !ok {
		return
	}
	path := commandPath(data)
	ix := b.newIx(e.ApplicationCommandInteraction, path)
	ix.cmd = &data
	spec, ok := b.commands[path]
	if !ok {
		ilog.Warn("Unknown command '%s'", path)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			ilog.Error("Error in command %s: %v\n%s", data.CommandName(), r, debug.Stack())
			ix.Send(Msg{Content: "An unexpected error occurred. Please try again later.", Ephemeral: true})
		}
	}()
	if spec.cooldown > 0 {
		k := cooldownKey{cmd: path, user: ix.User.ID}
		if spec.perGuild {
			k.guild = ix.GuildID
		}
		if wait, on := b.cooldown.hit(k, spec.cooldown); on {
			ilog.Info("User %s hit cooldown on %s", userTag(ix.User), lastWord(path))
			ix.Send(Msg{Content: fmt.Sprintf("This command is on cooldown. Please wait %.1f seconds.", wait.Seconds()), Ephemeral: true})
			return
		}
	}
	spec.handler(ix)
}

func lastWord(s string) string {
	if i := strings.LastIndexByte(s, ' '); i >= 0 {
		return s[i+1:]
	}
	return s
}

func (b *Bot) handleAutocomplete(e *events.AutocompleteInteractionCreate) {
	path := e.Data.CommandName
	if e.Data.SubCommandName != nil {
		path += " " + *e.Data.SubCommandName
	}
	ix := b.newIx(e.AutocompleteInteraction, path)
	fn, ok := b.autocompletes[path]
	if !ok {
		return
	}
	var focused string
	_ = json.Unmarshal(e.Data.Focused().Value, &focused)
	defer func() {
		if r := recover(); r != nil {
			ilog.Error("Ignoring exception in autocomplete for '%s': %v", path, r)
		}
	}()
	ix.Autocomplete(fn(ix, focused))
}

func (b *Bot) handleComponent(e *events.ComponentInteractionCreate) {
	cid := e.Data.CustomID()
	parts := strings.SplitN(cid, ":", 3)
	if len(parts) != 3 || parts[0] != "v" {
		return
	}
	v := b.ui.view(parts[1])
	if v == nil {
		return // expired, or from before a restart: ignored
	}
	idx, err := strconv.Atoi(parts[2])
	if err != nil {
		return
	}
	ix := b.newIx(e.ComponentInteraction, cid)
	if sel, ok := e.Data.(discord.StringSelectMenuInteractionData); ok {
		ix.values = sel.Values
	}
	defer func() {
		if r := recover(); r != nil {
			ilog.Error("Ignoring exception in view for item %s: %v\n%s", cid, r, debug.Stack())
		}
	}()
	v.dispatch(ix, idx)
}

func (b *Bot) handleModal(e *events.ModalSubmitInteractionCreate) {
	id, ok := strings.CutPrefix(e.Data.CustomID, "m:")
	if !ok {
		return
	}
	md := b.ui.takeModal(id)
	if md == nil {
		return
	}
	if md.timer != nil {
		md.timer.Stop()
	}
	ix := b.newIx(e.ModalSubmitInteraction, e.Data.CustomID)
	data := e.Data
	ix.modal = &data
	defer func() {
		if r := recover(); r != nil {
			ilog.Error("Ignoring exception in modal %s: %v\n%s", md.Title, r, debug.Stack())
		}
	}()
	md.onSubmit(ix)
}

// guildConfig reads the interaction guild's config.
func (ix *Ix) guildConfig() *db.GuildConfig { return ix.b.DB.GetGuildConfig(i64(ix.GuildID)) }
