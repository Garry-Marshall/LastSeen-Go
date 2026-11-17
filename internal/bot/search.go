package bot

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"lastseen/internal/db"
)

// /search: filter members and display or export them (cogs/commands.py).

const searchMaxResults = 1000

type cmpFilter struct {
	op    string // ">", "<" or "="
	value int64
}

type searchFilters struct {
	hasRoles bool // a roles filter was given (an empty list then matches nobody cached)
	roles    []snowflake.ID
	status   string // "" = no status filter
	inactive *cmpFilter
	activity *cmpFilter
	silent   *cmpFilter
	joined   *cmpFilter // value is the day's 00:00 UTC
	departed *cmpFilter
	username string
}

// searchResult is one matched member.
type searchResult struct {
	Username     string
	DisplayName  string
	UserID       int64
	Status       string
	LastSeen     *int64
	LastSeenStr  string
	JoinedAtStr  string
	JoinPosition *int64
	Roles        []string
	Activity30d  int64
	Activity7d   int64
	ActivityDay  int64
}

func (b *Bot) cmdSearch(ix *Ix) {
	gid := i64(ix.GuildID)
	cfg := b.DB.GetGuildConfig(gid)
	lang := guildLang(cfg)
	if admin, _ := b.isBotAdmin(ix, cfg); !admin {
		ix.Respond(Msg{Content: t("commands.search.admin_required", lang), Ephemeral: true})
		return
	}
	export := "none"
	if v, ok := ix.cmd.OptString("export"); ok {
		export = v
	}
	exportLower := strings.ToLower(export)
	if export != "" && exportLower != "none" && exportLower != "csv" && exportLower != "txt" {
		ix.Respond(Msg{Content: t("commands.search.invalid_export", lang, "export", export), Ephemeral: true})
		return
	}
	restricted := cfg != nil && cfg.AllowedChannels != nil && *cfg.AllowedChannels != ""
	if !isChannelAllowed(ix.ChannelID, cfg, ix.channelParentID()) {
		ix.Respond(Msg{Content: t("commands.search.channel_not_allowed", lang), Ephemeral: true})
		return
	}
	eph := !restricted
	ix.Defer(eph)

	opt := func(name string) string { v, _ := ix.cmd.OptString(name); return v }
	filters, err := b.parseSearchFilters(ix.GuildID, opt("roles"), opt("status"), opt("inactive"), opt("activity"),
		opt("silent"), opt("joined"), opt("departed"), opt("username"), lang)
	if err != nil {
		ix.Followup(Msg{Content: t("commands.search.invalid_filter", lang, "error", err.Error()), Ephemeral: true})
		return
	}

	members := b.DB.GetGuildMembers(gid, filters.departed != nil)
	if tracked := b.DB.GetTrackedUserIDs(gid); tracked != nil {
		keep := map[int64]bool{}
		for _, id := range tracked.IDs {
			keep[id] = true
		}
		filtered := members[:0]
		for _, m := range members {
			if keep[m.UserID] {
				filtered = append(filtered, m)
			}
		}
		members = filtered
	}
	var addedAt int64
	if cfg != nil {
		addedAt = cfg.AddedAt
	}

	activity := b.DB.GetGuildActivityTotals(gid, 30)
	var lastActive map[int64]int64
	if filters.silent != nil {
		lastActive = b.DB.GetGuildLastActive(gid)
	}
	var results []searchResult
	misses := 0
	for _, md := range members {
		dm, cached := b.Client.Caches.Member(ix.GuildID, sf(md.UserID))
		if !cached {
			misses++
			if filters.status != "" || len(filters.roles) > 0 {
				continue // these need Discord data
			}
			if matchesDBFilters(md, filters, addedAt, activity, lastActive) {
				results = append(results, dbOnlyResult(md, lang, activity))
			}
			continue
		}
		if b.matchesAllFilters(ix.GuildID, md, dm, filters, addedAt, activity, lastActive) {
			results = append(results, b.enrichResult(ix.GuildID, md, dm, lang, activity))
		}
	}
	if misses > 0 && filters.departed == nil {
		clog.Warn("Search had %d cache misses in guild %d", misses, gid)
	}
	if len(results) == 0 {
		ix.Followup(Msg{Content: t("commands.search.no_results", lang), Ephemeral: eph})
		return
	}

	// Most recent first: online now, then most recently offline, then never.
	recency := func(r searchResult) (int, int64) {
		switch {
		case r.LastSeen == nil:
			return 0, 0
		case *r.LastSeen == 0:
			return 2, 0
		}
		return 1, *r.LastSeen
	}
	sort.SliceStable(results, func(i, j int) bool {
		ai, av := recency(results[i])
		bi, bv := recency(results[j])
		if ai != bi {
			return ai > bi
		}
		return av > bv
	})
	if len(results) > searchMaxResults {
		ix.Followup(Msg{Content: t("commands.search.too_many", lang, "count", len(results), "max", searchMaxResults), Ephemeral: eph})
		results = results[:searchMaxResults]
	}

	switch exportLower {
	case "csv", "txt":
		var file *discord.File
		if exportLower == "csv" {
			file = discord.NewFile("member_search_"+time.Now().Format("20060102_150405")+".csv", "",
				strings.NewReader(searchCSV(results)))
		} else {
			file = discord.NewFile("member_search_"+time.Now().Format("20060102_150405")+".txt", "",
				strings.NewReader(searchTXT(results, filters)))
		}
		ix.Followup(Msg{Content: t("commands.search.export_success", lang, "count", len(results), "format", strings.ToUpper(exportLower)),
			Files: []*discord.File{file}, Ephemeral: eph})
	default:
		v, e := b.searchResultsView(results, filters, ix.User.ID, 15, lang)
		ix.Followup(Msg{Embeds: []discord.Embed{e}, View: v, Ephemeral: eph})
	}
}

// ---- filter parsing ----

var (
	filterValueRe = regexp.MustCompile(`^([<>=]?)(\d+)$`)
	filterDateRe  = regexp.MustCompile(`^([<>=]?)(\d{4}-\d{2}-\d{2})$`)
)

func (b *Bot) parseSearchFilters(guildID snowflake.ID, roles, status, inactive, activity, silent, joined, departed, username, lang string) (searchFilters, error) {
	var f searchFilters
	if roles != "" {
		var parts []string
		if strings.Contains(roles, "<@&") {
			parts = strings.Fields(roles)
		} else {
			for _, p := range strings.Split(roles, ",") {
				parts = append(parts, strings.TrimSpace(p))
			}
		}
		clog.Info("Role filter input: '%s' -> parsed as: %s", roles, pyStrList(parts))
		guildRoles := b.guildRoles(guildID)
		for _, rs := range parts {
			rs = strings.TrimSpace(rs)
			if rs == "" {
				continue
			}
			if strings.HasPrefix(rs, "<@&") && strings.HasSuffix(rs, ">") && len(rs) >= 4 {
				id, err := strconv.ParseInt(strings.TrimSpace(rs[3:len(rs)-1]), 10, 64)
				if err != nil {
					clog.Warn("  Invalid role mention format: %s", rs)
					continue
				}
				if r, ok := b.Client.Caches.Role(guildID, sf(id)); ok {
					f.roles = append(f.roles, r.ID)
					clog.Info("  Found role by mention: %s (ID: %d)", r.Name, r.ID)
				}
				continue
			}
			found := false
			for _, r := range guildRoles {
				if strings.EqualFold(r.Name, rs) {
					f.roles = append(f.roles, r.ID)
					clog.Info("  Found role by name: '%s' -> %s (ID: %d)", rs, r.Name, r.ID)
					found = true
					break
				}
			}
			if !found {
				clog.Warn("  Role not found: '%s'", rs)
			}
		}
		f.hasRoles = true
		if len(f.roles) > 0 {
			ids := make([]int64, len(f.roles))
			for i, r := range f.roles {
				ids[i] = i64(r)
			}
			clog.Info("Final role filter: %d role(s) - IDs: %s", len(ids), pyIntList(ids))
		} else {
			clog.Warn("No valid roles found in filter - will return 0 results")
		}
	}
	if status != "" {
		s := strings.ToLower(status)
		switch s {
		case "online", "offline", "idle", "dnd":
			f.status = s
		case "all":
		default:
			return f, errors.New(t("commands.search.err_status", lang))
		}
	}
	var err error
	parse := func(raw, unit string, dst **cmpFilter) {
		if raw == "" || err != nil {
			return
		}
		*dst, err = parseFilterValue(raw, unit, lang)
	}
	parse(inactive, "days", &f.inactive)
	parse(activity, "messages", &f.activity)
	parse(silent, "days", &f.silent)
	if err != nil {
		return f, err
	}
	if joined != "" {
		if f.joined, err = parseDateFilter(joined, lang); err != nil {
			return f, err
		}
	}
	if departed != "" {
		if f.departed, err = parseDateFilter(departed, lang); err != nil {
			return f, err
		}
	}
	if username != "" {
		f.username = strings.ToLower(username)
	}
	return f, nil
}

// pyStrList is repr() of a Python list of strings.
func pyStrList(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = pyReprStr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// pyReprStr is repr() of a str (quote choice as in Python).
func pyReprStr(s string) string {
	q := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = `"`
	}
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	s = r.Replace(s)
	if q == "'" {
		s = strings.ReplaceAll(s, "'", `\'`)
	}
	return q + s + q
}

func parseFilterValue(raw, unit, lang string) (*cmpFilter, error) {
	m := filterValueRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return nil, errors.New(t("commands.search.err_filter_format", lang, "unit", unit))
	}
	op := m[1]
	if op == "" {
		op = "="
	}
	v, perr := strconv.ParseInt(m[2], 10, 64)
	var shown any = v
	if perr != nil {
		shown, v = strings.TrimLeft(m[2], "0"), math.MaxInt64 // too big for int64: out of range anyway
	}
	if unit == "days" && (v < 0 || v > 36500) {
		return nil, errors.New(t("commands.search.err_days_range", lang, "value", shown))
	}
	if unit == "messages" && (v < 0 || v > 10000000) {
		return nil, errors.New(t("commands.search.err_messages_range", lang, "value", shown))
	}
	return &cmpFilter{op, v}, nil
}

func parseDateFilter(raw, lang string) (*cmpFilter, error) {
	m := filterDateRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return nil, errors.New(t("commands.search.err_date_format", lang))
	}
	op := m[1]
	if op == "" {
		op = "="
	}
	d, err := time.Parse("2006-01-02", m[2])
	if err != nil {
		return nil, errors.New(t("commands.search.err_date_invalid", lang, "date", m[2]))
	}
	if d.Year() < 2015 {
		return nil, errors.New(t("commands.search.err_date_min", lang))
	}
	if d.Year() > 2100 {
		return nil, errors.New(t("commands.search.err_date_max", lang))
	}
	return &cmpFilter{op, d.Unix()}, nil
}

func (c *cmpFilter) match(actual int64) bool {
	switch c.op {
	case ">":
		return actual > c.value
	case "<":
		return actual < c.value
	}
	return actual == c.value
}

// matchDay compares a timestamp with a date filter as whole UTC days.
func (c *cmpFilter) matchDay(ts int64) bool {
	switch c.op {
	case ">":
		return ts >= c.value+86400
	case "<":
		return ts < c.value
	}
	return c.value <= ts && ts < c.value+86400
}

// daysSince is int((now - since) // 86400) with a float now, like Python.
func daysSince(since int64) int64 {
	now := float64(time.Now().UnixMicro()) / 1e6
	return int64(math.Floor((now - float64(since)) / 86400))
}

func orZero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// matchesDBFilters checks the filters that need only stored data.
func matchesDBFilters(md *db.Member, f searchFilters, addedAt int64, activity map[int64]db.ActivityTotals, lastActive map[int64]int64) bool {
	if f.username != "" {
		if !strings.Contains(strings.ToLower(md.Username), f.username) &&
			!strings.Contains(strings.ToLower(strOr(md.Nickname, "")), f.username) {
			return false
		}
	}
	if f.inactive != nil {
		var days int64
		if md.LastSeen == nil || *md.LastSeen != 0 {
			since := orZero(md.LastSeen)
			if since == 0 {
				since = max(orZero(md.JoinDate), addedAt)
			}
			days = daysSince(since)
		}
		if !f.inactive.match(days) {
			return false
		}
	}
	if f.joined != nil {
		if !truthy(md.JoinDate) || !f.joined.matchDay(*md.JoinDate) {
			return false
		}
	}
	if f.departed != nil {
		if md.IsActive {
			return false
		}
		ts := orZero(md.LeftDate)
		if ts == 0 {
			ts = orZero(md.LastSeen)
		}
		if ts == 0 || !f.departed.matchDay(ts) {
			return false
		}
	}
	if f.activity != nil && !f.activity.match(activity[md.UserID].Total) {
		return false
	}
	if f.silent != nil {
		since := max(lastActive[md.UserID], orZero(md.JoinDate), addedAt)
		if !f.silent.match(daysSince(since)) {
			return false
		}
	}
	return true
}

func (b *Bot) matchesAllFilters(guildID snowflake.ID, md *db.Member, dm discord.Member, f searchFilters,
	addedAt int64, activity map[int64]db.ActivityTotals, lastActive map[int64]int64) bool {
	if !matchesDBFilters(md, f, addedAt, activity, lastActive) {
		return false
	}
	if f.hasRoles {
		if len(f.roles) == 0 {
			return false
		}
		has := map[snowflake.ID]bool{guildID: true}
		for _, r := range b.memberRoles(guildID, dm.RoleIDs) {
			has[r.ID] = true
		}
		any := false
		for _, r := range f.roles {
			if has[r] {
				any = true
				break
			}
		}
		if !any {
			return false
		}
	}
	if f.status != "" && statusName(b.statuses.Status(guildID, dm.User.ID)) != f.status {
		return false
	}
	return true
}

func dbOnlyResult(md *db.Member, lang string, activity map[int64]db.ActivityTotals) searchResult {
	a := activity[md.UserID]
	return searchResult{
		Username:     md.Username,
		UserID:       md.UserID,
		Status:       t("common.unknown", lang),
		LastSeen:     md.LastSeen,
		LastSeenStr:  formatRelativeTime(md.LastSeen, lang),
		JoinedAtStr:  formatRelativeTime(md.JoinDate, lang),
		JoinPosition: md.JoinPosition,
		Roles:        []string{},
		Activity30d:  a.Total,
		Activity7d:   a.ThisWeek,
		ActivityDay:  a.Today,
	}
}

func (b *Bot) enrichResult(guildID snowflake.ID, md *db.Member, dm discord.Member, lang string, activity map[int64]db.ActivityTotals) searchResult {
	status := statusName(b.statuses.Status(guildID, dm.User.ID))
	lastSeen := md.LastSeen
	if status != "offline" && lastSeen != nil && *lastSeen > 0 {
		zero := int64(0)
		lastSeen = &zero // online now, though the row still has the old time
	}
	var roles []string
	for _, r := range b.memberRoles(guildID, dm.RoleIDs) {
		if r.Name != "@everyone" {
			roles = append(roles, r.Name)
		}
	}
	a := activity[md.UserID]
	return searchResult{
		Username:     dm.User.Username,
		DisplayName:  memberDisplayName(dm),
		UserID:       i64(dm.User.ID),
		Status:       status,
		LastSeen:     lastSeen,
		LastSeenStr:  formatRelativeTime(lastSeen, lang),
		JoinedAtStr:  formatRelativeTime(md.JoinDate, lang),
		JoinPosition: md.JoinPosition,
		Roles:        roles,
		Activity30d:  a.Total,
		Activity7d:   a.ThisWeek,
		ActivityDay:  a.Today,
	}
}

// formatRelativeTime is the search's coarse "N days ago" text, using
// Python timedelta semantics (days floor, seconds 0..86399).
func formatRelativeTime(ts *int64, lang string) string {
	switch {
	case ts == nil:
		return t("common.never", lang)
	case *ts == 0:
		return t("commands.search.online_now", lang)
	case *ts < 0:
		return t("common.invalid_date", lang)
	}
	diff := time.Now().UnixMicro() - *ts*1_000_000
	const dayMicros = 86400 * 1_000_000
	days := diff / dayMicros
	if diff%dayMicros < 0 {
		days--
	}
	secs := (diff - days*dayMicros) / 1_000_000
	switch {
	case days > 365:
		return t("commands.search.years_ago", lang, "n", days/365)
	case days > 30:
		return t("commands.search.months_ago", lang, "n", days/30)
	case days > 0:
		return t("commands.search.days_ago", lang, "n", days)
	case secs > 3600:
		return t("commands.search.hours_ago", lang, "n", secs/3600)
	case secs > 60:
		return t("commands.search.minutes_ago", lang, "n", secs/60)
	}
	return t("commands.search.just_now", lang)
}

// ---- exports ----

func pyOptIntBlank(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}

func searchCSV(results []searchResult) string {
	rows := [][]string{{"Username", "Display Name", "User ID", "Status", "Last Seen", "Joined At", "Join Position",
		"Messages (30d)", "Messages (7d)", "Messages (Today)", "Roles", "Is Tracked"}}
	for _, r := range results {
		rows = append(rows, []string{
			sanitizeCSVValue(r.Username),
			sanitizeCSVValue(r.DisplayName),
			strconv.FormatInt(r.UserID, 10),
			r.Status,
			r.LastSeenStr,
			r.JoinedAtStr,
			pyOptIntBlank(r.JoinPosition),
			strconv.FormatInt(r.Activity30d, 10),
			strconv.FormatInt(r.Activity7d, 10),
			strconv.FormatInt(r.ActivityDay, 10),
			sanitizeCSVValue(strings.Join(r.Roles, ", ")),
			"Yes",
		})
	}
	return csvQuoteAll(rows)
}

// filterSummaryLines is the TXT export's filter list.
func filterSummaryLines(f searchFilters) []string {
	var lines []string
	if len(f.roles) > 0 {
		lines = append(lines, fmt.Sprintf("  • Roles: %d role(s)", len(f.roles)))
	}
	if f.status != "" {
		lines = append(lines, "  • Status: "+f.status)
	}
	if c := f.inactive; c != nil {
		lines = append(lines, fmt.Sprintf("  • Inactive: %s%d days", c.op, c.value))
	}
	if c := f.activity; c != nil {
		lines = append(lines, fmt.Sprintf("  • Activity: %s%d messages", c.op, c.value))
	}
	if c := f.silent; c != nil {
		lines = append(lines, fmt.Sprintf("  • Silent: %s%d days", c.op, c.value))
	}
	if c := f.joined; c != nil {
		lines = append(lines, "  • Joined: "+c.op+formatUTCDate(c.value))
	}
	if c := f.departed; c != nil {
		lines = append(lines, "  • Departed: "+c.op+formatUTCDate(c.value))
	}
	if f.username != "" {
		lines = append(lines, "  • Username contains: '"+f.username+"'")
	}
	return lines
}

func searchTXT(results []searchResult, f searchFilters) string {
	var sb strings.Builder
	sb.WriteString("Member Search Results - " + time.Now().UTC().Format("2006-01-02 15:04") + " UTC\n")
	fmt.Fprintf(&sb, "Total Members: %d\n", len(results))
	if lines := filterSummaryLines(f); len(lines) > 0 {
		sb.WriteString("\nFilters Applied:\n" + strings.Join(lines, "\n") + "\n")
	}
	sb.WriteString(strings.Repeat("=", 80) + "\n\n")
	for i, r := range results {
		fmt.Fprintf(&sb, "%d. %s", i+1, r.Username)
		if r.DisplayName != "" && r.DisplayName != r.Username {
			sb.WriteString(" (" + r.DisplayName + ")")
		}
		sb.WriteString("\n")
		fmt.Fprintf(&sb, "   User ID: %d\n", r.UserID)
		sb.WriteString("   Status: " + r.Status + "\n")
		sb.WriteString("   Last Seen: " + r.LastSeenStr + "\n")
		sb.WriteString("   Joined: " + r.JoinedAtStr + "\n")
		fmt.Fprintf(&sb, "   Activity: %d msgs (30d), %d msgs (7d), %d today\n", r.Activity30d, r.Activity7d, r.ActivityDay)
		if len(r.Roles) > 0 {
			sb.WriteString("   Roles: " + strings.Join(r.Roles, ", ") + "\n")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// ---- SearchResultsView ----

var searchStatusEmoji = map[string]string{"online": "🟢", "idle": "🟡", "dnd": "🔴", "offline": "⚫", "Unknown": "⚪"}

func (b *Bot) searchResultsView(results []searchResult, f searchFilters, ownerID snowflake.ID, perPage int, lang string) (*View, discord.Embed) {
	v := b.newView(300*time.Second).ownerOnly(ownerID, lang)
	perPage = max(1, perPage)
	page := 0
	maxPage := 0
	if len(results) > 0 {
		maxPage = (len(results) - 1) / perPage
	}

	filterText := func() string {
		var lines []string
		if len(f.roles) > 0 {
			lines = append(lines, t("commands.search_view.filter_roles", lang, "count", len(f.roles)))
		}
		if f.status != "" {
			lines = append(lines, t("commands.search_view.filter_status", lang, "status", f.status))
		}
		if c := f.inactive; c != nil {
			lines = append(lines, t("commands.search_view.filter_inactive", lang, "op", c.op, "value", c.value))
		}
		if c := f.activity; c != nil {
			lines = append(lines, t("commands.search_view.filter_activity", lang, "op", c.op, "value", c.value))
		}
		if c := f.silent; c != nil {
			lines = append(lines, t("commands.search_view.filter_silent", lang, "op", c.op, "value", c.value))
		}
		if c := f.joined; c != nil {
			lines = append(lines, t("commands.search_view.filter_joined", lang, "op", c.op, "date", formatUTCDate(c.value)))
		}
		if c := f.departed; c != nil {
			lines = append(lines, t("commands.search_view.filter_departed", lang, "op", c.op, "date", formatUTCDate(c.value)))
		}
		if f.username != "" {
			lines = append(lines, t("commands.search_view.filter_username", lang, "username", f.username))
		}
		if len(lines) == 0 {
			return t("commands.search_view.no_filters", lang)
		}
		return strings.Join(lines, "\n")
	}

	render := func() discord.Embed {
		if len(results) == 0 {
			return discord.Embed{Title: t("commands.search_view.title", lang),
				Description: t("commands.search_view.no_results_display", lang), Color: ColorBlue}
		}
		page = max(0, min(page, maxPage))
		start := page * perPage
		end := min(start+perPage, len(results))
		e := discord.Embed{Title: t("commands.search_view.title", lang),
			Description: t("commands.search_view.found", lang, "count", len(results)), Color: ColorBlue}
		ft := filterText()
		if runeLen(ft) > 1024 {
			ft = runeCut(ft, 1021) + "..."
		}
		e.Fields = append(e.Fields, field(t("commands.search_view.filters_applied", lang), ft, false))

		var lines []string
		for i, r := range results[start:end] {
			emoji, ok := searchStatusEmoji[r.Status]
			if !ok {
				emoji = "⚪"
			}
			username := runeCut(r.Username, 32)
			display := runeCut(r.DisplayName, 32)
			line := t("commands.search_view.result_line", lang, "i", start+i+1, "emoji", emoji, "username", username)
			if display != "" && display != username {
				line += t("commands.search_view.result_display", lang, "display", display)
			}
			line += t("commands.search_view.result_lastseen", lang, "last_seen", r.LastSeenStr)
			if r.Activity30d > 0 {
				line += t("commands.search_view.result_activity", lang, "count", r.Activity30d)
			}
			lines = append(lines, line)
		}
		text := strings.Join(lines, "\n\n")
		if runeLen(text) > 1024 {
			suffix := t("commands.search_view.truncated", lang)
			text = runeCut(text, 1024-runeLen(suffix)) + suffix
		}
		e.Fields = append(e.Fields, field(t("commands.search_view.members_field", lang), text, false))
		e.Footer = &discord.EmbedFooter{Text: t("commands.search_view.footer", lang, "page", page+1, "total", maxPage+1)}
		return e
	}

	var prev, next *Item
	updateButtons := func() {
		prev.Disabled = page == 0
		next.Disabled = page == maxPage
		if maxPage == 0 {
			prev.Disabled, next.Disabled = true, true
		}
	}
	prev = v.Button(t("commands.search_view.btn_prev", lang), discord.ButtonStylePrimary, "", -1, func(ix *Ix) {
		page = max(0, page-1)
		updateButtons()
		ix.Update(Msg{Embeds: []discord.Embed{render()}, View: v})
	})
	next = v.Button(t("commands.search_view.btn_next", lang), discord.ButtonStylePrimary, "", -1, func(ix *Ix) {
		page = min(maxPage, page+1)
		updateButtons()
		ix.Update(Msg{Embeds: []discord.Embed{render()}, View: v})
	})
	v.Button(t("commands.search_view.btn_export_csv", lang), discord.ButtonStyleSuccess, "", -1, func(ix *Ix) {
		ix.DeferUpdate()
		name := "member_search_" + time.Now().UTC().Format("20060102_150405") + ".csv"
		ix.Followup(Msg{Content: t("commands.search.export_success", lang, "count", len(results), "format", "CSV"),
			Files: []*discord.File{discord.NewFile(name, "", strings.NewReader(searchCSV(results)))}, Ephemeral: true})
	})
	v.Button(t("commands.search_view.btn_export_txt", lang), discord.ButtonStyleSuccess, "", -1, func(ix *Ix) {
		ix.DeferUpdate()
		name := "member_search_" + time.Now().UTC().Format("20060102_150405") + ".txt"
		ix.Followup(Msg{Content: t("commands.search.export_success", lang, "count", len(results), "format", "TXT"),
			Files: []*discord.File{discord.NewFile(name, "", strings.NewReader(searchTXT(results, f)))}, Ephemeral: true})
	})
	updateButtons()
	return v, render()
}
