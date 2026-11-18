package bot

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"lastseen/internal/db"
	"lastseen/internal/locale"
	"lastseen/internal/logx"
	"lastseen/internal/tzdb"
)

// /config with its panel, dialogs and quick-setup wizard, and /help.

var alog = logx.Get("admin")

const (
	defaultUserRoleName = "LastSeen User"
	everyoneRoleName    = "@everyone"
)

// detailedHelp lists the commands with a `/help <command>` detail page.
var detailedHelp = []string{"watch", "search"}

// ==================== Shared helpers ====================

// userRoleOf is guild_config.get('user_role_name', 'LastSeen User').
func userRoleOf(cfg *db.GuildConfig) string {
	if cfg == nil {
		return defaultUserRoleName
	}
	return cfg.UserRoleName
}

// checkAdminPermission is permissions.check_admin_permission: true for bot
// admins; otherwise it answers with the no-permission error.
func (b *Bot) checkAdminPermission(ix *Ix, guildID snowflake.ID) bool {
	cfg := b.DB.GetGuildConfig(i64(guildID))
	role := adminRoleName(cfg)
	if hasBotAdminRole(ix.memberCtx(), role) {
		return true
	}
	lang := guildLang(cfg)
	ix.Respond(embedMsg(errorEmbed(t("errors.no_permission", lang, "role", role), lang), true))
	return false
}

// parseIntInput parses a whole number typed into a dialog: surrounding
// whitespace, a sign and single underscores between digits are accepted. A
// number too large for int64 still parses but yields -1, which no channel,
// day or hour matches.
func parseIntInput(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	body := strings.TrimLeft(s, "+-")
	if len(s)-len(body) > 1 || body == "" || body[0] == '_' || body[len(body)-1] == '_' || strings.Contains(body, "__") {
		return 0, false
	}
	for _, c := range body {
		if (c < '0' || c > '9') && c != '_' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 10, 64)
	if err != nil {
		return -1, true
	}
	return n, true
}

// channelNoThread returns a cached channel of the guild, but never a thread.
func (b *Bot) channelNoThread(guildID snowflake.ID, id int64) (discord.GuildChannel, bool) {
	ch, ok := b.guildChannel(guildID, sf(id))
	if !ok || isThreadType(ch.Type()) {
		return nil, false
	}
	return ch, true
}

func isThreadType(ct discord.ChannelType) bool {
	return ct == discord.ChannelTypeGuildPublicThread || ct == discord.ChannelTypeGuildPrivateThread ||
		ct == discord.ChannelTypeGuildNewsThread
}

// isTextChannel reports whether ch is a text or announcement channel.
func isTextChannel(ch discord.GuildChannel) bool {
	return ch.Type() == discord.ChannelTypeGuildText || ch.Type() == discord.ChannelTypeGuildNews
}

// channelTypeName names a (non-thread) channel's type for the "not a text
// channel" error, e.g. "VoiceChannel".
func channelTypeName(ch discord.GuildChannel) string {
	switch ch.Type() {
	case discord.ChannelTypeGuildText, discord.ChannelTypeGuildNews:
		return "TextChannel"
	case discord.ChannelTypeGuildVoice:
		return "VoiceChannel"
	case discord.ChannelTypeGuildCategory:
		return "CategoryChannel"
	case discord.ChannelTypeGuildStageVoice:
		return "StageChannel"
	case discord.ChannelTypeGuildForum, discord.ChannelTypeGuildMedia:
		return "ForumChannel"
	}
	return "GuildChannel"
}

// textChannels is guild.text_channels: sorted by (position, id).
func (b *Bot) textChannels(guildID snowflake.ID) []discord.GuildChannel {
	var out []discord.GuildChannel
	for ch := range b.Client.Caches.ChannelsForGuild(guildID) {
		if isTextChannel(ch) {
			out = append(out, ch)
		}
	}
	slices.SortStableFunc(out, func(a, c discord.GuildChannel) int {
		if a.Position() != c.Position() {
			return a.Position() - c.Position()
		}
		return cmp.Compare(a.ID(), c.ID())
	})
	return out
}

// textChannelByName is discord.utils.get(guild.text_channels, name=name).
func (b *Bot) textChannelByName(guildID snowflake.ID, name string) (discord.GuildChannel, bool) {
	for _, ch := range b.textChannels(guildID) {
		if ch.Name() == name {
			return ch, true
		}
	}
	return nil, false
}

// resolveChannelInput parses a typed channel: a <#mention>, then an ID, then
// a text channel name (with or without #).
func (b *Bot) resolveChannelInput(guildID snowflake.ID, s string) (discord.GuildChannel, bool) {
	if strings.HasPrefix(s, "<#") && strings.HasSuffix(s, ">") {
		if id, ok := parseIntInput(s[2 : len(s)-1]); ok {
			if ch, ok := b.channelNoThread(guildID, id); ok {
				return ch, true
			}
		}
	}
	if id, ok := parseIntInput(s); ok {
		if ch, ok := b.channelNoThread(guildID, id); ok {
			return ch, true
		}
	}
	return b.textChannelByName(guildID, strings.ToLower(strings.TrimLeft(s, "#")))
}

// guildHasRole is discord.utils.get(guild.roles, name=name) is not None.
func (b *Bot) guildHasRole(guildID snowflake.ID, name string) bool {
	for _, r := range b.guildRoles(guildID) {
		if r.Name == name {
			return true
		}
	}
	return false
}

// defaultRoleName is guild.default_role.name.
func (b *Bot) defaultRoleName(guildID snowflake.ID) string {
	if r, ok := b.Client.Caches.Role(guildID, guildID); ok {
		return r.Name
	}
	return everyoneRoleName
}

// capitalizeWord capitalizes a report frequency: "weekly" -> "Weekly".
func capitalizeWord(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// reportSchedule is the localized schedule line shared by the reports modal
// (prefix "channel_config.reports") and the config overview
// ("admin.view_config").
func reportSchedule(prefix, frequency string, dayWeekly, dayMonthly, hour int, tz, lang string) string {
	switch frequency {
	case "weekly":
		return t(prefix+".schedule_weekly", lang, "day", locale.Weekday(dayWeekly, lang), "daynum", dayWeekly+1,
			"hour", hour, "tz", tz)
	case "monthly":
		return t(prefix+".schedule_monthly", lang, "day", dayMonthly, "hour", hour, "tz", tz)
	}
	return t(prefix+".schedule_both", lang, "day", locale.Weekday(dayWeekly, lang), "daynum", dayWeekly+1,
		"monthday", dayMonthly, "hour", hour, "tz", tz)
}

// ==================== /config and /help ====================

func (b *Bot) cmdConfig(ix *Ix) {
	if !b.checkAdminPermission(ix, ix.GuildID) {
		return
	}
	cfg := ix.guildConfig()
	lang := guildLang(cfg)
	e := newEmbed(t("admin.config.title", lang), ColorGold)
	e.Description = t("admin.config.description", lang)
	ix.Respond(Msg{Embeds: []discord.Embed{e}, View: b.configView(ix.GuildID, cfg), Ephemeral: true})
	alog.Info("User %s opened config panel in guild %s", userTag(ix.User), ix.GuildName())
}

func (b *Bot) helpAutocomplete(ix *Ix, current string) []discord.AutocompleteChoice {
	cur := strings.TrimLeft(strings.ToLower(current), "/")
	var out []discord.AutocompleteChoice
	for _, c := range detailedHelp {
		if strings.Contains(c, cur) {
			out = append(out, discord.AutocompleteChoiceString{Name: c, Value: c})
		}
	}
	return out
}

func (b *Bot) cmdHelp(ix *Ix) {
	cfg := ix.guildConfig()
	adminRole, userRole := adminRoleName(cfg), userRoleOf(cfg)
	mc := ix.memberCtx()
	isAdmin := hasBotAdminRole(mc, adminRole)
	lang := guildLang(cfg)

	// Respect the user_role_required toggle (only when the guild has a config row).
	if cfg != nil && !canUseBotCommands(mc, cfg) {
		ix.Respond(embedMsg(errorEmbed(t("errors.no_user_or_admin_permission", lang, "role", userRole), lang), true))
		return
	}

	if command, _ := ix.cmd.OptString("command"); command != "" {
		key := strings.ToLower(strings.TrimLeft(strings.TrimSpace(command), "/"))
		if slices.Contains(detailedHelp, key) {
			if !isAdmin {
				ix.Respond(embedMsg(errorEmbed(t("errors.no_permission", lang, "role", adminRole), lang), true))
				return
			}
			ix.Respond(embedMsg(helpDetailEmbed(key, lang), true))
			alog.Info("User %s viewed /help %s in guild %s", userTag(ix.User), key, ix.GuildName())
			return
		}
		ix.Respond(embedMsg(errorEmbed(t("admin.help.detail_unknown", lang, "command", key), lang), true))
		return
	}

	e := newEmbed(t("admin.help.title", lang), ColorBlue)
	e.Description = t("admin.help.description", lang)
	e.Fields = append(e.Fields,
		field(t("admin.help.links_title", lang), t("admin.help.links", lang), false),
		field(t("admin.help.user_commands_title", lang), t("admin.help.user_commands", lang), false))
	if isAdmin {
		e.Fields = append(e.Fields,
			field(t("admin.help.admin_commands_title", lang), t("admin.help.admin_commands", lang), false),
			field(t("admin.help.search_title", lang), t("admin.help.search", lang), false),
			field(t("admin.help.watch_section_title", lang), t("admin.help.watch_section", lang), false))
		e.Footer = &discord.EmbedFooter{Text: t("admin.help.footer_admin", lang, "role", adminRole)}
	} else {
		e.Footer = &discord.EmbedFooter{Text: t("admin.help.footer_user", lang, "role", userRole)}
	}
	ix.Respond(embedMsg(e, true))
	alog.Info("User %s viewed help in guild %s", userTag(ix.User), ix.GuildName())
}

// helpDetailEmbed is _build_watch_help / _build_search_help.
func helpDetailEmbed(key, lang string) discord.Embed {
	p := "admin.help." + key + "_detail_"
	middle := "commands" // watch: commands; search: filters
	if key == "search" {
		middle = "filters"
	}
	e := newEmbed(t(p+"title", lang), ColorBlurple)
	e.Description = t(p+"desc", lang)
	e.Fields = []discord.EmbedField{
		field(t(p+middle+"_title", lang), t(p+middle, lang), false),
		field(t(p+"examples_title", lang), t(p+"examples", lang), false),
		field(t(p+"notes_title", lang), t(p+"notes", lang), false),
	}
	return e
}

// ==================== ConfigView ====================

func (b *Bot) configView(guildID snowflake.ID, cfg *db.GuildConfig) *View {
	lang := guildLang(cfg)
	v := b.newView(300 * time.Second)
	label := func(k string) string { return t("admin.config_view.btn_"+k, lang) }
	fresh := func() *db.GuildConfig { return b.DB.GetGuildConfig(i64(guildID)) }
	// modalButton opens a dialog built from the current config, admins only.
	modalButton := func(key, emoji string, row int, build func(cfg *db.GuildConfig) *Modal) {
		v.Button(label(key), discord.ButtonStylePrimary, emoji, row, func(ix *Ix) {
			if !b.checkAdminPermission(ix, guildID) {
				return
			}
			ix.SendModal(build(fresh()))
		})
	}

	modalButton("set_channel", "📢", 0, func(c *db.GuildConfig) *Modal { return b.channelModal(guildID, c) })
	modalButton("set_inactive_days", "📅", 0, func(c *db.GuildConfig) *Modal { return b.inactiveDaysModal(guildID, c) })
	modalButton("set_timezone", "🌍", 0, func(c *db.GuildConfig) *Modal { return b.timezoneModal(guildID, c) })
	v.Button(label("set_language"), discord.ButtonStylePrimary, "🌐", 0, func(ix *Ix) {
		if !b.checkAdminPermission(ix, guildID) {
			return
		}
		e := newEmbed(t("admin.language.prompt_title", lang), ColorBlurple)
		e.Description = t("admin.language.prompt_desc", lang)
		ix.Respond(Msg{Embeds: []discord.Embed{e}, View: b.languageSelectView(guildID, fresh()), Ephemeral: true})
	})

	modalButton("set_bot_admin_role", "👑", 1, func(c *db.GuildConfig) *Modal { return b.botAdminRoleModal(guildID, c) })
	modalButton("set_user_role", "👤", 1, func(c *db.GuildConfig) *Modal { return b.userRoleModal(guildID, c) })
	toggleLabel := func(on bool) (string, discord.ButtonStyle) {
		state, style := "common.off", discord.ButtonStyleDanger
		if on {
			state, style = "common.on", discord.ButtonStyleSuccess
		}
		return t("admin.config_view.toggle_user_role", lang, "state", t(state, lang)), style
	}
	var toggle *Item
	toggle = v.Button("", discord.ButtonStyleSecondary, "🔐", 1, func(ix *Ix) {
		if !b.checkAdminPermission(ix, guildID) {
			return
		}
		cur := fresh()
		if cur == nil {
			ix.Respond(embedMsg(errorEmbed(t("admin.errors.guild_config_not_found", lang), lang), true))
			return
		}
		newValue := !cur.UserRoleRequired
		if !b.DB.SetUserRoleRequired(i64(guildID), newValue, ix.GuildName()) {
			ix.Respond(embedMsg(errorEmbed(t("admin.config_view.toggle_failed", lang), lang), true))
			return
		}
		toggle.Label, toggle.Style = toggleLabel(newValue)
		ix.Update(Msg{View: v})
		key := "admin.config_view.user_role_disabled"
		if newValue {
			key = "admin.config_view.user_role_enabled"
		}
		ix.Followup(embedMsg(successEmbed(t(key, lang), lang), true))
		alog.Info("User role requirement toggled to %t in guild %s", newValue, ix.GuildName())
	})
	toggle.Label, toggle.Style = toggleLabel(cfg != nil && cfg.UserRoleRequired)

	modalButton("set_track_only_roles", "🎯", 2, func(c *db.GuildConfig) *Modal { return b.trackOnlyRolesModal(guildID, c) })
	modalButton("set_allowed_channels", "📝", 2, func(c *db.GuildConfig) *Modal { return b.allowedChannelsModal(guildID, c) })

	modalButton("configure_reports", "📊", 3, func(c *db.GuildConfig) *Modal { return b.reportsConfigModal(guildID, c) })
	v.Button(label("disable_reports"), discord.ButtonStyleDanger, "⏹️", 3, func(ix *Ix) {
		if !b.checkAdminPermission(ix, guildID) {
			return
		}
		cur := fresh()
		if cur == nil || cur.ReportFrequency == nil || *cur.ReportFrequency == "" {
			ix.Respond(embedMsg(errorEmbed(t("admin.config_view.reports_not_enabled", lang), lang), true))
			return
		}
		e := newEmbed(t("admin.config_view.confirm_disable_title", lang), ColorOrange)
		e.Description = t("admin.config_view.confirm_disable_desc", lang)
		ix.Respond(Msg{Embeds: []discord.Embed{e}, View: b.disableReportsConfirmView(guildID, cur), Ephemeral: true})
	})

	v.Button(label("quick_setup"), discord.ButtonStylePrimary, "", 4, func(ix *Ix) {
		if !b.checkAdminPermission(ix, guildID) {
			return
		}
		view, embed := b.quickSetupView(guildID, fresh())
		ix.Respond(Msg{Embeds: []discord.Embed{embed}, View: view, Ephemeral: true})
		alog.Info("Quick setup wizard started for guild %s", ix.GuildName())
	})
	v.Button(label("view_config"), discord.ButtonStyleSecondary, "⚙️", 4, func(ix *Ix) {
		b.viewConfig(ix, guildID, lang)
	})
	return v
}

// viewConfig answers with the current configuration overview.
func (b *Bot) viewConfig(ix *Ix, guildID snowflake.ID, lang string) {
	cfg := b.DB.GetGuildConfig(i64(guildID))
	if cfg == nil {
		ix.Respond(embedMsg(errorEmbed(t("admin.errors.guild_config_not_found", lang), lang), true))
		return
	}
	e := newEmbed(t("admin.view_config.title", lang), ColorBlue)
	add := func(key, value string) {
		e.Fields = append(e.Fields, field(t("admin.view_config."+key, lang), value, false))
	}
	channelText := func(id int64) string {
		if _, ok := b.channelNoThread(guildID, id); ok {
			return mentionChannel(id)
		}
		return t("common.unknown_with_id", lang, "id", id)
	}

	if cfg.NotificationChannelID != nil && *cfg.NotificationChannelID != 0 {
		add("notification_channel", channelText(*cfg.NotificationChannelID))
	} else {
		add("notification_channel", t("common.not_set", lang))
	}
	add("inactive_days", t("admin.view_config.days_value", lang, "days", cfg.InactiveDays))
	add("timezone", cfg.Timezone)

	if cfg.ReportFrequency != nil && *cfg.ReportFrequency != "" {
		channelMention := "Unknown (" + intOrNone(cfg.ReportChannelID) + ")"
		if cfg.ReportChannelID != nil && *cfg.ReportChannelID != 0 {
			if _, ok := b.channelNoThread(guildID, *cfg.ReportChannelID); ok {
				channelMention = mentionChannel(*cfg.ReportChannelID)
			}
		}
		typesStr := t("admin.view_config.types_none", lang)
		if cfg.ReportTypes != nil && *cfg.ReportTypes != "" {
			if types, ok := db.LoadStrings(*cfg.ReportTypes); !ok {
				alog.Error("Failed to parse report_types JSON for guild %d", guildID)
				typesStr = t("admin.view_config.types_invalid", lang)
			} else if len(types) > 0 {
				typesStr = strings.Join(types, ", ")
			}
		}
		schedule := reportSchedule("admin.view_config", *cfg.ReportFrequency, cfg.ReportDayWeekly,
			cfg.ReportDayMonthly, cfg.ReportTimeHour, cfg.Timezone, lang)
		add("scheduled_reports", t("admin.view_config.reports_enabled_value", lang,
			"channel", channelMention, "schedule", schedule, "types", typesStr))
	} else {
		add("scheduled_reports", t("admin.view_config.reports_disabled_value", lang))
	}

	add("bot_admin_role", cfg.BotAdminRoleName)
	if cfg.UserRoleRequired {
		add("user_role_required", t("common.yes", lang))
	} else {
		add("user_role_required", t("common.no", lang))
	}
	add("user_role_name", cfg.UserRoleName)

	trackStr := t("admin.view_config.all_roles", lang)
	if cfg.TrackOnlyRoles != nil && *cfg.TrackOnlyRoles != "" {
		if roles, ok := db.LoadStrings(*cfg.TrackOnlyRoles); !ok {
			alog.Error("Failed to parse track_only_roles JSON")
			trackStr = t("admin.view_config.error_parsing_roles", lang)
		} else if len(roles) > 0 {
			trackStr = joinCapped(roles, lang, 1024, ", ")
		}
	}
	add("track_only_roles", trackStr)

	allowedStr := t("admin.view_config.all_channels", lang)
	if cfg.AllowedChannels != nil && *cfg.AllowedChannels != "" {
		if ids, ok := db.LoadInts(*cfg.AllowedChannels); !ok {
			alog.Error("Failed to parse allowed_channels JSON")
			allowedStr = t("admin.view_config.error_parsing_channels", lang)
		} else if len(ids) > 0 {
			mentions := make([]string, len(ids))
			for i, id := range ids {
				mentions[i] = channelText(id)
			}
			allowedStr = joinCapped(mentions, lang, 1024, ", ")
		}
	}
	add("allowed_channels", allowedStr)

	add("tracked_members", strconv.Itoa(len(b.DB.GetAllGuildMembers(i64(guildID)))))
	ix.Respond(embedMsg(e, true))
}

// disableReportsConfirmView is DisableReportsConfirmView.
func (b *Bot) disableReportsConfirmView(guildID snowflake.ID, cfg *db.GuildConfig) *View {
	lang := guildLang(cfg)
	v := b.newView(60 * time.Second)
	v.Button(t("admin.config_view.btn_confirm_disable", lang), discord.ButtonStyleDanger, "✅", -1, func(ix *Ix) {
		e := errorEmbed(t("admin.config_view.disable_failed", lang), lang)
		if b.DB.DisableReports(i64(guildID)) {
			e = successEmbed(t("admin.config_view.reports_disabled_success", lang), lang)
		}
		ix.Update(Msg{Embeds: []discord.Embed{e}, ClearView: true})
		v.Stop()
	})
	v.Button(t("common.cancel", lang), discord.ButtonStyleSecondary, "❌", -1, func(ix *Ix) {
		e := newEmbed(t("common.cancelled", lang), ColorBlue)
		e.Description = t("admin.config_view.disable_cancelled_desc", lang)
		ix.Update(Msg{Embeds: []discord.Embed{e}, ClearView: true})
		v.Stop()
	})
	return v
}

// languageSelectView is LanguageSelectView: one option per loaded locale.
func (b *Bot) languageSelectView(guildID snowflake.ID, cfg *db.GuildConfig) *View {
	lang := guildLang(cfg)
	current := "en"
	if cfg != nil {
		current = cfg.Language
	}
	var options []discord.StringSelectMenuOption
	for _, code := range locale.Available() {
		options = append(options, discord.StringSelectMenuOption{Label: locale.LanguageName(code), Value: code, Default: code == current})
	}
	v := b.newView(120 * time.Second)
	v.Select(t("admin.language.select_placeholder", lang), options, 1, 1, -1, func(ix *Ix) {
		newLang := ix.Values()[0]
		if !b.DB.SetGuildLanguage(i64(guildID), newLang, ix.GuildName()) {
			ix.Update(Msg{Embeds: []discord.Embed{errorEmbed(t("admin.language.update_failed", newLang), newLang)}, ClearView: true})
			return
		}
		msg := t("admin.language.set", newLang, "language", locale.LanguageName(newLang))
		ix.Update(Msg{Embeds: []discord.Embed{successEmbed(msg, newLang)}, ClearView: true})
		alog.Info("Language set to '%s' in guild %s", newLang, ix.GuildName())
	})
	return v
}

// ==================== Channel and timing modals ====================

// channelModal is ChannelModal: the member-leave notification channel.
func (b *Bot) channelModal(guildID snowflake.ID, cfg *db.GuildConfig) *Modal {
	lang := guildLang(cfg)
	var input string
	md := b.newModal(t("channel_config.channel.modal_title", lang), func(ix *Ix) {
		s := strings.TrimSpace(ix.Text(input))
		if s == "" {
			if b.DB.SetNotificationChannel(i64(guildID), nil, ix.GuildName()) {
				ix.Respond(embedMsg(successEmbed(t("channel_config.channel.cleared", lang), lang), true))
				alog.Info("Notification channel cleared in guild %s", ix.GuildName())
			} else {
				ix.Respond(embedMsg(errorEmbed(t("channel_config.channel.update_failed", lang), lang), true))
			}
			return
		}
		ch, ok := b.resolveChannelInput(guildID, s)
		if !ok {
			ix.Respond(embedMsg(errorEmbed(t("channel_config.channel.not_found", lang, "channel", s), lang), true))
			return
		}
		if !isTextChannel(ch) {
			ix.Respond(embedMsg(errorEmbed(t("channel_config.channel.invalid_type", lang,
				"channel", ch.Name(), "type", channelTypeName(ch)), lang), true))
			return
		}
		id := i64(ch.ID())
		if b.DB.SetNotificationChannel(i64(guildID), &id, ix.GuildName()) {
			ix.Respond(embedMsg(successEmbed(t("channel_config.channel.set", lang, "channel", mentionChannel(id)), lang), true))
			alog.Info("Notification channel set to %s in guild %s", ch.Name(), ix.GuildName())
		} else {
			ix.Respond(embedMsg(errorEmbed(t("channel_config.channel.update_failed", lang), lang), true))
		}
	})
	md.TextDisplay(t("channel_config.channel.info", lang))
	def := ""
	if cfg != nil && cfg.NotificationChannelID != nil && *cfg.NotificationChannelID != 0 {
		def = mentionChannel(*cfg.NotificationChannelID)
	}
	input = md.Input(TextInput{
		Label:       t("channel_config.channel.input_label", lang),
		Placeholder: t("channel_config.channel.input_placeholder", lang),
		Default:     def,
		MaxLength:   100,
	})
	return md
}

// inactiveDaysModal is InactiveDaysModal.
func (b *Bot) inactiveDaysModal(guildID snowflake.ID, cfg *db.GuildConfig) *Modal {
	lang := guildLang(cfg)
	var input string
	md := b.newModal(t("channel_config.inactive_days.modal_title", lang), func(ix *Ix) {
		days, ok := parseIntInput(strings.TrimSpace(ix.Text(input)))
		if !ok {
			ix.Respond(embedMsg(errorEmbed(t("channel_config.inactive_days.invalid", lang), lang), true))
			return
		}
		if days < 1 || days > 365 {
			ix.Respond(embedMsg(errorEmbed(t("channel_config.inactive_days.range", lang), lang), true))
			return
		}
		if b.DB.SetInactiveDays(i64(guildID), int(days), ix.GuildName()) {
			ix.Respond(embedMsg(successEmbed(t("channel_config.inactive_days.set", lang, "days", days), lang), true))
			alog.Info("Inactive days threshold set to %d in guild %s", days, ix.GuildName())
		} else {
			ix.Respond(embedMsg(errorEmbed(t("channel_config.inactive_days.update_failed", lang), lang), true))
		}
	})
	md.TextDisplay(t("channel_config.inactive_days.info", lang))
	def := ""
	if cfg != nil && cfg.InactiveDays != 0 {
		def = strconv.Itoa(cfg.InactiveDays)
	}
	input = md.Input(TextInput{
		Label:       t("channel_config.inactive_days.input_label", lang),
		Placeholder: t("channel_config.inactive_days.input_placeholder", lang),
		Default:     def,
		Required:    true,
		MaxLength:   3,
	})
	return md
}

// timezoneModal asks for the server's time zone, checked against the known
// zone names (tzdb).
func (b *Bot) timezoneModal(guildID snowflake.ID, cfg *db.GuildConfig) *Modal {
	lang := guildLang(cfg)
	var input string
	md := b.newModal(t("channel_config.timezone.modal_title", lang), func(ix *Ix) {
		tz := strings.TrimSpace(ix.Text(input))
		if !tzdb.Valid(tz) {
			msg := t("channel_config.timezone.invalid", lang, "tz", tz)
			if suggestions := tzdb.Suggestions(tz, 5); len(suggestions) > 0 {
				lines := make([]string, len(suggestions))
				for i, s := range suggestions {
					lines[i] = t("channel_config.timezone.suggestion_line", lang, "tz", s)
				}
				msg += t("channel_config.timezone.did_you_mean", lang, "suggestions", strings.Join(lines, "\n"))
			} else {
				msg += t("channel_config.timezone.no_suggestions", lang)
			}
			ix.Respond(embedMsg(errorEmbed(msg, lang), true))
			return
		}
		b.DB.SetTimezone(i64(guildID), tz, ix.GuildName())
		ix.Respond(embedMsg(successEmbed(t("channel_config.timezone.set", lang, "tz", tz), lang), true))
		alog.Info("Timezone set to %s for guild %s", tz, ix.GuildName())
	})
	md.TextDisplay(t("channel_config.timezone.info", lang))
	def := ""
	if cfg != nil {
		def = cfg.Timezone
	}
	input = md.Input(TextInput{
		Label:       t("channel_config.timezone.input_label", lang),
		Placeholder: t("channel_config.timezone.input_placeholder", lang),
		Default:     def,
		Required:    true,
		MaxLength:   50,
	})
	return md
}

var validReportTypes = []string{"activity", "members", "departures", "retention"}

// reportsConfigModal is ReportsConfigModal: five labelled fields (a
// TextDisplay would exceed Discord's 5-component modal limit).
func (b *Bot) reportsConfigModal(guildID snowflake.ID, cfg *db.GuildConfig) *Modal {
	lang := guildLang(cfg)
	p := "channel_config.reports."
	var chIn, freqIn, typesIn, daysIn, hourIn string
	fail := func(ix *Ix, key string, kv ...any) {
		ix.Respond(embedMsg(errorEmbed(t(p+key, lang, kv...), lang), true))
	}
	md := b.newModal(t(p+"modal_title", lang), func(ix *Ix) {
		chStr := strings.TrimSpace(ix.Text(chIn))
		ch, ok := b.resolveChannelInput(guildID, chStr)
		if !ok || !isTextChannel(ch) {
			fail(ix, "channel_not_found", "channel", chStr)
			return
		}
		frequency := strings.ToLower(strings.TrimSpace(ix.Text(freqIn)))
		if frequency != "weekly" && frequency != "monthly" && frequency != "both" {
			fail(ix, "invalid_frequency")
			return
		}
		var types []string
		for _, s := range strings.Split(strings.ToLower(strings.TrimSpace(ix.Text(typesIn))), ",") {
			if s = strings.TrimSpace(s); s != "" && slices.Contains(validReportTypes, s) {
				types = append(types, s)
			}
		}
		if len(types) == 0 {
			fail(ix, "invalid_types")
			return
		}

		dayWeekly, dayMonthly, hour := 0, 1, 9
		if daysStr := strings.TrimSpace(ix.Text(daysIn)); daysStr != "" {
			parts := strings.Split(daysStr, ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			if parts[0] != "" {
				n, ok := parseIntInput(parts[0])
				if !ok {
					fail(ix, "weekly_day_nan")
					return
				}
				if n < 1 || n > 7 {
					fail(ix, "weekly_day_range")
					return
				}
				dayWeekly = int(n) - 1 // user 1-7 → internal 0-6
			}
			if len(parts) >= 2 && parts[1] != "" {
				n, ok := parseIntInput(parts[1])
				if !ok {
					fail(ix, "monthly_day_nan")
					return
				}
				if n < 1 || n > 28 {
					fail(ix, "monthly_day_range")
					return
				}
				dayMonthly = int(n)
			}
		}
		if hourStr := strings.TrimSpace(ix.Text(hourIn)); hourStr != "" {
			n, ok := parseIntInput(hourStr)
			if !ok {
				fail(ix, "hour_nan")
				return
			}
			if n < 0 || n > 23 {
				fail(ix, "hour_range")
				return
			}
			hour = int(n)
		}

		if !b.DB.SetReportConfig(i64(guildID), i64(ch.ID()), frequency, types, dayWeekly, dayMonthly, hour, ix.GuildName()) {
			fail(ix, "save_failed")
			return
		}
		tz := "UTC"
		if cur := b.DB.GetGuildConfig(i64(guildID)); cur != nil {
			tz = cur.Timezone
		}
		schedule := reportSchedule("channel_config.reports", frequency, dayWeekly, dayMonthly, hour, tz, lang)
		ix.Respond(embedMsg(successEmbed(t(p+"success", lang, "channel", mentionChannel(i64(ch.ID())),
			"frequency", capitalizeWord(frequency), "types", strings.Join(types, ", "), "schedule", schedule), lang), true))
		alog.Info("Reports configured for guild %s: %s to %s at %02d:00 %s", ix.GuildName(), frequency, ch.Name(), hour, tz)
	})

	chDef, freqDef, typesDef, daysDef, hourDef := "", "", strings.Join(validReportTypes, ","), "", "9"
	if cfg != nil {
		if cfg.ReportChannelID != nil && *cfg.ReportChannelID != 0 {
			chDef = strconv.FormatInt(*cfg.ReportChannelID, 10)
		}
		if cfg.ReportFrequency != nil && *cfg.ReportFrequency != "" {
			freqDef = *cfg.ReportFrequency
		}
		if cfg.ReportTypes != nil && *cfg.ReportTypes != "" {
			if types, ok := db.LoadStrings(*cfg.ReportTypes); ok {
				typesDef = strings.Join(types, ",")
			} else {
				alog.Error("Failed to parse report_types JSON for guild %d", guildID)
			}
		}
		daysDef = strconv.Itoa(cfg.ReportDayWeekly+1) + "," + strconv.Itoa(cfg.ReportDayMonthly)
		hourDef = strconv.Itoa(cfg.ReportTimeHour)
	}
	chIn = md.Input(TextInput{Label: t(p+"channel_label", lang), Description: t(p+"channel_label_desc", lang),
		Placeholder: t(p+"channel_placeholder", lang), Default: chDef, Required: true, MaxLength: 100})
	freqIn = md.Input(TextInput{Label: t(p+"frequency_label", lang),
		Placeholder: t(p+"frequency_placeholder", lang), Default: freqDef, Required: true, MaxLength: 10})
	typesIn = md.Input(TextInput{Label: t(p+"types_label", lang), Description: t(p+"types_label_desc", lang),
		Placeholder: t(p+"types_placeholder", lang), Default: typesDef, Required: true, MaxLength: 50})
	daysIn = md.Input(TextInput{Label: t(p+"days_label", lang),
		Placeholder: t(p+"days_placeholder", lang), Default: daysDef, MaxLength: 10})
	hourIn = md.Input(TextInput{Label: t(p+"hour_label", lang),
		Placeholder: t(p+"hour_placeholder", lang), Default: hourDef, MaxLength: 2})
	return md
}

// ==================== Role modals ====================

// roleNameModal is BotAdminRoleModal (admin=true) or UserRoleModal.
func (b *Bot) roleNameModal(guildID snowflake.ID, cfg *db.GuildConfig, admin bool) *Modal {
	lang := guildLang(cfg)
	p, kind, current := "admin.role.user_", "User role", userRoleOf(cfg)
	set := b.DB.SetUserRoleName
	if admin {
		p, kind, current = "admin.role.bot_admin_", "Bot admin role", adminRoleName(cfg)
		set = b.DB.SetBotAdminRole
	}
	var input string
	md := b.newModal(t(p+"modal_title", lang), func(ix *Ix) {
		name := strings.TrimSpace(ix.Text(input))
		fail := func(msg string) { ix.Respond(embedMsg(errorEmbed(msg, lang), true)) }
		if name == "" {
			fail(t("admin.role.name_empty", lang))
			return
		}
		if runeLen(name) > 100 {
			fail(t("admin.role.name_too_long", lang))
			return
		}
		// @everyone would make every member a bot admin (and is ignored by
		// the admin check anyway), so it is refused.
		if admin && strings.EqualFold(name, b.defaultRoleName(guildID)) {
			fail(t("admin.role.everyone_not_allowed", lang))
			return
		}
		if !b.guildHasRole(guildID, name) {
			// A warning, not an error: the setting is saved anyway.
			fail(t("admin.role.not_exist_warning", lang, "role", name))
			set(i64(guildID), name, ix.GuildName())
			alog.Info("%s set to '%s' in guild %s (role doesn't exist yet)", kind, name, ix.GuildName())
			return
		}
		if set(i64(guildID), name, ix.GuildName()) {
			okKey := "admin.role.user_set"
			if admin {
				okKey = "admin.role.bot_admin_set"
			}
			ix.Respond(embedMsg(successEmbed(t(okKey, lang, "role", name), lang), true))
			alog.Info("%s set to '%s' in guild %s", kind, name, ix.GuildName())
		} else {
			fail(t(p+"update_failed", lang))
		}
	})
	infoKey := "admin.role.user_info"
	if admin {
		infoKey = "admin.role.bot_admin_info"
	}
	md.TextDisplay(t(infoKey, lang))
	input = md.Input(TextInput{
		Label:       t(p+"input_label", lang),
		Placeholder: t(p+"input_placeholder", lang),
		Default:     current,
		Required:    true,
		MaxLength:   100,
	})
	return md
}

func (b *Bot) botAdminRoleModal(guildID snowflake.ID, cfg *db.GuildConfig) *Modal {
	return b.roleNameModal(guildID, cfg, true)
}

func (b *Bot) userRoleModal(guildID snowflake.ID, cfg *db.GuildConfig) *Modal {
	return b.roleNameModal(guildID, cfg, false)
}

// splitCommaList is [s.strip() for s in text.split(',') if s.strip()].
func splitCommaList(text string) []string {
	var out []string
	for _, s := range strings.Split(text, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// trackOnlyRolesModal is TrackOnlyRolesModal (empty = track all roles).
func (b *Bot) trackOnlyRolesModal(guildID snowflake.ID, cfg *db.GuildConfig) *Modal {
	lang := guildLang(cfg)
	p := "admin.track_roles."
	var input string
	md := b.newModal(t(p+"modal_title", lang), func(ix *Ix) {
		text := strings.TrimSpace(ix.Text(input))
		if text == "" {
			if b.DB.SetTrackOnlyRoles(i64(guildID), nil, ix.GuildName()) {
				ix.Respond(embedMsg(successEmbed(t(p+"cleared", lang), lang), true))
				alog.Info("Track only roles cleared in guild %s", ix.GuildName())
			} else {
				ix.Respond(embedMsg(errorEmbed(t(p+"update_failed", lang), lang), true))
			}
			return
		}
		names := splitCommaList(text)
		if len(names) == 0 {
			ix.Respond(embedMsg(errorEmbed(t(p+"no_names", lang), lang), true))
			return
		}
		var missing []string
		for _, n := range names {
			if !b.guildHasRole(guildID, n) {
				missing = append(missing, n)
			}
		}
		if !b.DB.SetTrackOnlyRoles(i64(guildID), names, ix.GuildName()) {
			ix.Respond(embedMsg(errorEmbed(t(p+"update_failed", lang), lang), true))
			return
		}
		msg := t(p+"set", lang, "roles", strings.Join(names, ", "))
		if len(missing) > 0 {
			msg += t(p+"set_warning", lang, "roles", strings.Join(missing, ", "))
		}
		ix.Respond(embedMsg(successEmbed(msg, lang), true))
		alog.Info("Track only roles set to %q in guild %s", names, ix.GuildName())
	})
	md.TextDisplay(t(p+"info", lang))
	def := ""
	if roles := cfg.TrackOnlyRoleList(); len(roles) > 0 {
		def = strings.Join(roles, ", ")
	}
	input = md.Input(TextInput{
		Label:       t(p+"input_label", lang),
		Placeholder: t(p+"input_placeholder", lang),
		Default:     def,
		Paragraph:   true,
		MaxLength:   4000, // Discord's maximum: a longer prefill would stop the dialog opening
	})
	return md
}

// ==================== Allowed channels modal ====================

// allowedChannelsModal is AllowedChannelsModal (empty = every channel).
func (b *Bot) allowedChannelsModal(guildID snowflake.ID, cfg *db.GuildConfig) *Modal {
	lang := guildLang(cfg)
	p := "admin.allowed_channels."
	var input string
	md := b.newModal(t(p+"modal_title", lang), func(ix *Ix) {
		text := strings.TrimSpace(ix.Text(input))
		if text == "" {
			if b.DB.SetAllowedChannels(i64(guildID), nil, ix.GuildName()) {
				ix.Respond(embedMsg(successEmbed(t(p+"cleared", lang), lang), true))
				alog.Info("Allowed channels cleared in guild %s", ix.GuildName())
			} else {
				ix.Respond(embedMsg(errorEmbed(t(p+"update_failed", lang), lang), true))
			}
			return
		}
		idents := splitCommaList(text)
		if len(idents) == 0 {
			ix.Respond(embedMsg(errorEmbed(t(p+"no_identifiers", lang), lang), true))
			return
		}
		var ids []int64
		var missing []string
		for _, ident := range idents {
			var ch discord.GuildChannel
			var ok bool
			if id, isInt := parseIntInput(ident); isInt {
				ch, ok = b.channelNoThread(guildID, id)
			} else {
				ch, ok = b.textChannelByName(guildID, strings.ToLower(strings.TrimLeft(ident, "#")))
			}
			if ok {
				ids = append(ids, i64(ch.ID()))
			} else {
				missing = append(missing, ident)
			}
		}
		if len(ids) == 0 {
			ix.Respond(embedMsg(errorEmbed(t(p+"none_found", lang), lang), true))
			return
		}
		if !b.DB.SetAllowedChannels(i64(guildID), ids, ix.GuildName()) {
			ix.Respond(embedMsg(errorEmbed(t(p+"update_failed", lang), lang), true))
			return
		}
		var mentions []string
		for _, id := range ids {
			if _, ok := b.channelNoThread(guildID, id); ok {
				mentions = append(mentions, mentionChannel(id))
			}
		}
		msg := t(p+"set", lang, "channels", strings.Join(mentions, ", "))
		if len(missing) > 0 {
			msg += t(p+"set_warning", lang, "channels", strings.Join(missing, ", "))
		}
		ix.Respond(embedMsg(successEmbed(msg, lang), true))
		alog.Info("Allowed channels set to %v in guild %s", ids, ix.GuildName())
	})
	md.TextDisplay(t(p+"info", lang))
	input = md.Input(TextInput{
		Label:       t(p+"input_label", lang),
		Placeholder: t(p+"input_placeholder", lang),
		Default:     b.currentChannelsText(guildID, cfg),
		Paragraph:   true,
		MaxLength:   4000,
	})
	return md
}

// currentChannelsText renders the stored allowed channels so that submitting
// them unchanged reads back the same channels: "#name" for a text channel
// with a unique name, the ID otherwise.
func (b *Bot) currentChannelsText(guildID snowflake.ID, cfg *db.GuildConfig) string {
	if cfg == nil || cfg.AllowedChannels == nil || *cfg.AllowedChannels == "" {
		return ""
	}
	ids, ok := db.LoadInts(*cfg.AllowedChannels)
	if !ok {
		return ""
	}
	nameCount := map[string]int{}
	for _, ch := range b.textChannels(guildID) {
		nameCount[ch.Name()]++
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		if ch, ok := b.channelNoThread(guildID, id); ok && isTextChannel(ch) && nameCount[ch.Name()] == 1 {
			parts[i] = "#" + ch.Name()
		} else {
			parts[i] = strconv.FormatInt(id, 10)
		}
	}
	return strings.Join(parts, ", ")
}

// ==================== Quick setup wizard ====================

const quickSetupSteps = 5

// quickSetupView is QuickSetupView: a 5-step wizard (channel, inactive days,
// admin role, timezone, summary). It returns the view and the first step.
func (b *Bot) quickSetupView(guildID snowflake.ID, cfg *db.GuildConfig) (*View, discord.Embed) {
	lang := guildLang(cfg)
	p := "admin.quick_setup."
	v := b.newView(600 * time.Second)
	step := 0

	currentChannel := func() string {
		if cfg != nil && cfg.NotificationChannelID != nil && *cfg.NotificationChannelID != 0 {
			return mentionChannel(*cfg.NotificationChannelID)
		}
		return t("common.not_set", lang)
	}
	stepEmbed := func() discord.Embed {
		var e discord.Embed
		n := strconv.Itoa(step + 1)
		switch step {
		case 0:
			e = newEmbed(t(p+"step1_title", lang), ColorBlue)
			e.Description = t(p+"step1_desc", lang, "channel", currentChannel())
		case 1:
			days := 10
			if cfg != nil {
				days = cfg.InactiveDays
			}
			e = newEmbed(t(p+"step2_title", lang), ColorBlue)
			e.Description = t(p+"step2_desc", lang, "days", days)
		case 2:
			e = newEmbed(t(p+"step3_title", lang), ColorBlue)
			e.Description = t(p+"step3_desc", lang, "role", adminRoleName(cfg))
		case 3:
			tz := "UTC"
			if cfg != nil {
				tz = cfg.Timezone
			}
			e = newEmbed(t(p+"step4_title", lang), ColorGold)
			e.Description = t(p+"step4_desc", lang, "timezone", tz)
		default:
			e = newEmbed(t(p+"step5_title", lang), ColorGreen)
			if cfg != nil {
				e.Description = t(p+"step5_summary", lang, "channel", currentChannel(), "days", cfg.InactiveDays,
					"role", cfg.BotAdminRoleName, "timezone", cfg.Timezone)
			} else {
				e.Description = t(p+"step5_no_config", lang)
			}
		}
		e.Footer = &discord.EmbedFooter{Text: t(p+"step"+n+"_footer", lang)}
		return e
	}

	var prev, next, skip, configure *Item
	updateButtons := func() {
		prev.Disabled = step == 0
		if step == quickSetupSteps-1 {
			next.Label, next.Style = t(p+"btn_finish", lang), discord.ButtonStyleSuccess
		} else {
			next.Label, next.Style = t(p+"btn_next", lang), discord.ButtonStylePrimary
		}
		configure.Disabled = step == quickSetupSteps-1
		skip.Disabled = step != 2 && step != 3 // only the optional steps
	}
	// show re-reads the config (a step's dialog may have changed it) and
	// redraws the wizard.
	show := func(ix *Ix) {
		updateButtons()
		cfg = b.DB.GetGuildConfig(i64(guildID))
		ix.Update(Msg{Embeds: []discord.Embed{stepEmbed()}, View: v})
	}

	prev = v.Button(t(p+"btn_prev", lang), discord.ButtonStyleSecondary, "", 0, func(ix *Ix) {
		step = max(0, step-1)
		show(ix)
	})
	next = v.Button("", discord.ButtonStylePrimary, "", 0, func(ix *Ix) {
		if step == quickSetupSteps-1 {
			ix.Update(Msg{Embeds: []discord.Embed{successEmbed(t(p+"finished", lang), lang)}, ClearView: true})
			alog.Info("Quick setup completed for guild %s", ix.GuildName())
			return
		}
		step = min(quickSetupSteps-1, step+1)
		show(ix)
	})
	skip = v.Button(t(p+"btn_skip", lang), discord.ButtonStyleSecondary, "", 0, func(ix *Ix) {
		step = min(quickSetupSteps-1, step+1)
		show(ix)
	})
	configure = v.Button(t(p+"btn_configure", lang), discord.ButtonStyleSuccess, "", 1, func(ix *Ix) {
		cfg = b.DB.GetGuildConfig(i64(guildID)) // the dialog prefills from the current settings
		var md *Modal
		switch step {
		case 0:
			md = b.channelModal(guildID, cfg)
		case 1:
			md = b.inactiveDaysModal(guildID, cfg)
		case 2:
			md = b.botAdminRoleModal(guildID, cfg)
		case 3:
			md = b.timezoneModal(guildID, cfg)
		}
		if md != nil {
			ix.SendModal(md)
		}
	})
	v.Button(t(p+"btn_cancel", lang), discord.ButtonStyleDanger, "", 1, func(ix *Ix) {
		e := newEmbed(t(p+"cancelled_title", lang), ColorRed)
		e.Description = t(p+"cancelled_desc", lang)
		ix.Update(Msg{Embeds: []discord.Embed{e}, ClearView: true})
		alog.Info("Quick setup cancelled for guild %s", ix.GuildName())
	})
	updateButtons()
	return v, stepEmbed()
}
