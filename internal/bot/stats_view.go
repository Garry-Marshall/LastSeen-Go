package bot

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"lastseen/internal/db"
	"lastseen/internal/locale"
	"lastseen/internal/pyfmt"
)

// /user-stats: the admin analytics dashboard (cogs/commands.py).

func (b *Bot) cmdUserStats(ix *Ix) {
	gid := i64(ix.GuildID)
	cfg := b.DB.GetGuildConfig(gid)
	lang := guildLang(cfg)
	if admin, role := b.isBotAdmin(ix, cfg); !admin {
		ix.Respond(embedMsg(errorEmbed(t("errors.no_permission", lang, "role", role), lang), true))
		return
	}
	restricted := cfg != nil && cfg.AllowedChannels != nil && *cfg.AllowedChannels != ""
	if !isChannelAllowed(ix.ChannelID, cfg, ix.channelParentID()) {
		ix.Respond(Msg{Content: t("commands.search.channel_not_allowed", lang), Ephemeral: true})
		return
	}
	eph := !restricted
	ix.Defer(eph)

	e, ok := b.statsOverview(ix.GuildID, lang)
	if !ok {
		ix.Followup(Msg{Content: t("commands.user_stats.stats_failed", lang), Ephemeral: eph})
		return
	}
	ix.Followup(Msg{Embeds: []discord.Embed{e}, View: b.userStatsView(ix.GuildID, ix.User.ID, lang), Ephemeral: eph})
	clog.Info("User %s viewed user-stats in guild %s", userTag(ix.User), ix.GuildName())
}

// statsOverview builds the overview embed (false when the stats failed).
func (b *Bot) statsOverview(guildID snowflake.ID, lang string) (discord.Embed, bool) {
	gid := i64(guildID)
	stats := b.DB.GetServerSnapshotStats(gid)
	if stats == nil {
		return discord.Embed{}, false
	}
	growthRate := 0.0
	if g := b.DB.GetMemberGrowthStats(gid, 30, nil); g != nil {
		growthRate = g.GrowthRate
	}
	return b.statsOverviewEmbed(gid, stats, growthRate, lang), true
}

func signIndicator(v float64, up, down, flat string) string {
	switch {
	case v > 0:
		return up
	case v < 0:
		return down
	}
	return flat
}

// bar is "█"*filled + "░"*(length-filled).
func bar(filled, length int) string {
	filled = max(0, min(filled, length))
	return strings.Repeat("█", filled) + strings.Repeat("░", length-filled)
}

func (b *Bot) statsOverviewEmbed(gid int64, s *db.SnapshotStats, growthRate float64, lang string) discord.Embed {
	e := newEmbed(t("commands.user_stats.overview_title", lang), ColorBlue)
	growthText := t("commands.user_stats.no_change", lang)
	if growthRate != 0 {
		growthText = t("commands.user_stats.vs_last_month", lang,
			"indicator", signIndicator(growthRate, "📈", "📉", "➡️"), "pct", abs(growthRate))
	}
	current := s.ActiveMembers
	var activePct, inactivePct any = int64(0), int64(0)
	if current > 0 {
		activePct = float64(s.Active30d) / float64(current) * 100
		inactivePct = float64(s.Inactive30d) / float64(current) * 100
	}
	e.Description = t("commands.user_stats.overview_desc", lang, "total", current, "growth", growthText,
		"active", s.Active30d, "active_pct", activePct, "inactive", s.Inactive30d, "inactive_pct", inactivePct)

	e.Fields = append(e.Fields, field(t("commands.user_stats.this_month_title", lang),
		t("commands.user_stats.this_month_value", lang, "joins", s.JoinsThisMonth, "leaves", s.LeavesThisMonth,
			"indicator", signIndicator(float64(s.NetGrowth), "🔼", "🔽", "➡️"), "net", s.NetGrowth), false))
	e.Fields = append(e.Fields, field(t("commands.user_stats.activity_title", lang),
		t("commands.user_stats.activity_value", lang, "total", s.TotalMessages30d, "avg", s.AvgMessagesPerMember,
			"user", s.MostActiveUser, "count", s.MostActiveCount), false))

	a := b.DB.GetActivityStats(gid, nil)
	maxOffline := max(a.Offline1h, a.Offline24h, a.Offline7d, a.Offline30d, a.Offline30dPlus, 1)
	row := func(label string, n int64) string {
		return label + " " + bar(int(float64(n)/float64(maxOffline)*20), 20) + " " + pyfmt.MustValue(n, ">5,") + "\n"
	}
	chart := "```\n" + row("<1 hour ", a.Offline1h) + row("<24 hrs ", a.Offline24h) + row("<7 days ", a.Offline7d) +
		row("<30 days", a.Offline30d) + row("30+ days", a.Offline30dPlus) + "```"
	e.Fields = append(e.Fields, field(t("commands.user_stats.distribution_title", lang), chart, false))

	seg := b.DB.GetParticipationSegments(gid, 30, nil)
	if seg.Lurkers != 0 || seg.Ghosts != 0 {
		e.Fields = append(e.Fields, field(t("commands.user_stats.participation_title", lang),
			t("commands.user_stats.participation_value", lang, "lurkers", seg.Lurkers,
				"pct", pyfmt.Round(seg.LurkerPct), "ghosts", seg.Ghosts), false))
	}
	e.Footer = &discord.EmbedFooter{Text: t("commands.user_stats.overview_footer", lang)}
	return e
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// userStatsView is UserStatsView: the dashboard's panel buttons.
func (b *Bot) userStatsView(guildID snowflake.ID, ownerID snowflake.ID, lang string) *View {
	gid := i64(guildID)
	v := b.newView(300*time.Second).ownerOnly(ownerID, lang)
	panel := func(build func() discord.Embed) func(ix *Ix) {
		return func(ix *Ix) {
			ix.DeferUpdate()
			ix.EditOriginal(Msg{Embeds: []discord.Embed{build()}, View: b.backView(guildID, ownerID, lang)})
		}
	}
	primary := discord.ButtonStylePrimary
	v.Button(t("commands.stats_view.btn_retention", lang), primary, "", 0, panel(func() discord.Embed {
		return retentionEmbed(b.DB.GetRetentionCohorts(gid, nil), b.DB.GetActivationFunnel(gid, nil), b.DB.GetDepartureLifespan(gid, nil), lang)
	}))
	v.Button(t("commands.stats_view.btn_growth", lang), primary, "", 0, panel(func() discord.Embed {
		return growthEmbed(b.DB.GetMemberGrowthStats(gid, 30, nil), b.DB.GetMemberGrowthStats(gid, 90, nil),
			b.DB.GetMemberGrowthStats(gid, 365, nil), lang)
	}))
	v.Button(t("commands.stats_view.btn_leaderboard", lang), primary, "", 1, func(ix *Ix) {
		ix.DeferUpdate()
		lv, e := b.leaderboardView(guildID, ownerID, lang)
		ix.EditOriginal(Msg{Embeds: []discord.Embed{e}, View: lv})
	})
	v.Button(t("commands.stats_view.btn_health", lang), primary, "", 0, panel(func() discord.Embed {
		return healthEmbed(b.DB.GetServerHealth(gid, nil), b.DB.GetHealthHistory(gid, 12, 7), lang)
	}))
	v.Button(t("commands.stats_view.btn_heatmap", lang), primary, "", 1, panel(func() discord.Embed {
		tz := "UTC"
		if cfg := b.DB.GetGuildConfig(gid); cfg != nil {
			tz = cfg.Timezone
		}
		return heatmapEmbed(b.DB.GetServerActivityWindows(gid, 30, tz, nil), tz, lang)
	}))
	v.Button(t("commands.stats_view.btn_participation", lang), primary, "", 1, panel(func() discord.Embed {
		since := nowUnix() - 30*86400
		return participationEmbed(b.DB.GetParticipationSegments(gid, 30, nil), b.DB.GetLurkers(gid, 30, 15),
			b.DB.GetGhosts(gid, 30, 15), b.DB.CountReturns(gid, since), b.DB.GetRecentReturns(gid, since, 5), lang)
	}))
	v.Button(t("commands.stats_view.btn_lifecycle", lang), primary, "", 1, panel(func() discord.Embed {
		return lifecycleEmbed(b.DB.GetLifecycleSegments(gid), lang)
	}))
	v.Button(t("commands.stats_view.btn_export", lang), discord.ButtonStyleSuccess, "", 1, func(ix *Ix) {
		ix.DeferUpdate()
		report, ok := b.statsReport(gid)
		if !ok {
			ix.Followup(Msg{Content: t("commands.search_view.export_failed", lang, "error", "no statistics available"), Ephemeral: true})
			return
		}
		name := "server_stats_" + time.Now().UTC().Format("20060102_150405") + ".txt"
		ix.Followup(Msg{Content: t("commands.stats_view.export_success", lang),
			Files: []*discord.File{discord.NewFile(name, "", strings.NewReader(report))}, Ephemeral: true})
	})
	return v
}

// backView is the one-button view returning to the overview.
func (b *Bot) backView(guildID, ownerID snowflake.ID, lang string) *View {
	v := b.newView(300*time.Second).ownerOnly(ownerID, lang)
	v.Button(t("commands.stats_view.btn_back", lang), discord.ButtonStyleSecondary, "", -1, func(ix *Ix) {
		ix.DeferUpdate()
		b.returnToOverview(ix, guildID, ownerID, lang)
	})
	return v
}

func (b *Bot) returnToOverview(ix *Ix, guildID, ownerID snowflake.ID, lang string) {
	e, ok := b.statsOverview(guildID, lang)
	if !ok {
		clog.Error("Failed to return to overview: no statistics for guild %d", guildID)
		ix.Followup(Msg{Content: t("commands.stats_view.error", lang, "error", "no statistics available"), Ephemeral: true})
		return
	}
	ix.EditOriginal(Msg{Embeds: []discord.Embed{e}, View: b.userStatsView(guildID, ownerID, lang)})
}

// statsReport is the Export Report text file.
func (b *Bot) statsReport(gid int64) (string, bool) {
	s := b.DB.GetServerSnapshotStats(gid)
	if s == nil {
		return "", false
	}
	g30 := b.DB.GetMemberGrowthStats(gid, 30, nil)
	if g30 == nil {
		g30 = &db.GrowthStats{}
	}
	board := b.DB.GetActivityLeaderboard(gid, 30, 25)
	line := strings.Repeat("=", 80) + "\n"
	dash := strings.Repeat("-", 80) + "\n"
	var sb strings.Builder
	sb.WriteString(line + "SERVER STATISTICS REPORT\n")
	sb.WriteString("Generated: " + time.Now().UTC().Format("2006-01-02 15:04") + " UTC\n")
	sb.WriteString(line + "\n")
	sb.WriteString("OVERVIEW\n" + dash)
	fmt.Fprintf(&sb, "Total Members:     %d\n", s.ActiveMembers)
	fmt.Fprintf(&sb, "Active (30d):      %d\n", s.Active30d)
	fmt.Fprintf(&sb, "Inactive (30d):    %d\n\n", s.Inactive30d)
	sb.WriteString("GROWTH (30 DAYS)\n" + dash)
	fmt.Fprintf(&sb, "Joins:             %d\n", g30.Joins)
	fmt.Fprintf(&sb, "Leaves:            %d\n", g30.Leaves)
	fmt.Fprintf(&sb, "Net Growth:        %d\n", g30.NetGrowth)
	fmt.Fprintf(&sb, "Growth Rate:       %s%%\n\n", pyfmt.MustValue(g30.GrowthRate, ".2f"))
	sb.WriteString("ACTIVITY (30 DAYS)\n" + dash)
	fmt.Fprintf(&sb, "Total Messages:    %d\n", s.TotalMessages30d)
	fmt.Fprintf(&sb, "Avg per Member:    %s\n\n", pyfmt.MustValue(s.AvgMessagesPerMember, ".1f"))
	sb.WriteString("TOP 25 MOST ACTIVE MEMBERS\n" + dash)
	sb.WriteString(pyfmt.MustValue("Rank", "<6") + " " + pyfmt.MustValue("Username", "<30") + " " +
		pyfmt.MustValue("Display Name", "<30") + " " + pyfmt.MustValue("Messages", "<10") + "\n")
	sb.WriteString(dash)
	for i, m := range board {
		display := strOr(m.Nickname, m.Username)
		sb.WriteString(pyfmt.MustValue(int64(i+1), "<6") + " " + pyfmt.MustValue(runeCut(m.Username, 28), "<30") + " " +
			pyfmt.MustValue(runeCut(display, 28), "<30") + " " + pyfmt.MustValue(m.TotalMessages, "<10") + "\n")
	}
	return sb.String(), true
}

// ---- panels ----

func retentionEmbed(cohorts map[string]db.Cohort, funnel db.ActivationFunnel, life db.DepartureLifespan, lang string) discord.Embed {
	e := newEmbed(t("commands.stats_view.retention_title", lang), ColorPurple)
	e.Description = t("commands.stats_view.retention_desc", lang)
	names := map[string]string{
		"30d": t("commands.stats_view.retention_period_30d", lang),
		"60d": t("commands.stats_view.retention_period_60d", lang),
		"90d": t("commands.stats_view.retention_period_90d", lang),
	}
	anyJoined := false
	for _, period := range db.CohortPeriods {
		c, ok := cohorts[period]
		if !ok || c.TotalJoined <= 0 {
			continue
		}
		anyJoined = true
		e.Fields = append(e.Fields, field(t("commands.stats_view.period_field", lang, "period", names[period]),
			t("commands.stats_view.retention_field_value", lang, "joined", c.TotalJoined, "active", c.StillActive,
				"recently", c.ActiveRecently, "rate", c.RetentionRate), false))
	}
	if !anyJoined {
		e.Description = t("commands.stats_view.retention_no_data", lang)
	}

	if len(funnel.Checkpoints) > 0 {
		var lines []string
		for _, cp := range funnel.Checkpoints {
			lines = append(lines, pyfmt.MustValue(cp.Label, "<4")+bar(int(cp.Rate/100*20), 20)+" "+
				pyfmt.MustValue(cp.Rate, ">3.0f")+"%  (n="+pyfmt.MustValue(cp.Matured, ",")+")")
		}
		value := t("commands.stats_view.funnel_intro", lang) + "\n```\n" + strings.Join(lines, "\n") + "\n```"
		if d := funnel.LargestDropoff; d != nil {
			value += t("commands.stats_view.funnel_dropoff", lang, "from_label", d[0], "to_label", d[1])
		}
		e.Fields = append(e.Fields, field(t("commands.stats_view.funnel_title", lang), value, false))
	} else {
		e.Fields = append(e.Fields, field(t("commands.stats_view.funnel_title", lang), t("commands.stats_view.funnel_no_data", lang), false))
	}

	if life.Sample >= 3 {
		bk := life.Buckets
		labels := []string{"<1 day", "<7 days", "<30 days", "<90 days", "90+ days"}
		counts := []int64{bk.Under1d, bk.Under7d, bk.Under30d, bk.Under90d, bk.Over90d}
		maxCount := int64(1)
		if m := max(counts[0], counts[1], counts[2], counts[3], counts[4]); m > 0 {
			maxCount = m
		}
		var lines []string
		for i, label := range labels {
			lines = append(lines, pyfmt.MustValue(label, "<8")+" "+bar(int(float64(counts[i])/float64(maxCount)*20), 20)+" "+
				pyfmt.MustValue(counts[i], ">4"))
		}
		value := t("commands.stats_view.lifespan_summary", lang, "sample", life.Sample, "median", life.MedianDays, "avg", life.AvgDays)
		value += "\n```\n" + strings.Join(lines, "\n") + "\n```"
		value += t("commands.stats_view.lifespan_early_churn", lang, "pct", pyfmt.Round(life.EarlyChurnPct))
		e.Fields = append(e.Fields, field(t("commands.stats_view.lifespan_title", lang), value, false))
	} else {
		e.Fields = append(e.Fields, field(t("commands.stats_view.lifespan_title", lang), t("commands.stats_view.lifespan_no_data", lang), false))
	}
	return e
}

func growthEmbed(g30, g90, g365 *db.GrowthStats, lang string) discord.Embed {
	e := newEmbed(t("commands.stats_view.growth_title", lang), ColorGreen)
	for _, p := range []struct {
		name string
		g    *db.GrowthStats
	}{
		{t("commands.stats_view.growth_period_30", lang), g30},
		{t("commands.stats_view.growth_period_90", lang), g90},
		{t("commands.stats_view.growth_period_365", lang), g365},
	} {
		if p.g == nil {
			continue
		}
		e.Fields = append(e.Fields, field(t("commands.stats_view.period_field", lang, "period", p.name),
			t("commands.stats_view.growth_field_value", lang, "joins", p.g.Joins, "leaves", p.g.Leaves,
				"net", p.g.NetGrowth, "indicator", signIndicator(p.g.GrowthRate, "📈", "📉", "➡️"), "rate", abs(p.g.GrowthRate)), true))
	}
	return e
}

func heatmapEmbed(w db.ActivityWindows, tz, lang string) discord.Embed {
	e := newEmbed(t("commands.stats_view.heatmap_title", lang), ColorOrange)
	if w.Total == 0 {
		e.Description = t("commands.stats_view.heatmap_no_data", lang)
		return e
	}
	maxDay := w.ByWeekday[0]
	for _, c := range w.ByWeekday {
		maxDay = max(maxDay, c)
	}
	names := make([]string, 7)
	width := 0
	for d := range 7 {
		names[d] = locale.Weekday(d, lang)
		width = max(width, runeLen(names[d]))
	}
	var lines []string
	for d := range 7 {
		n := 0
		if maxDay > 0 {
			n = int(float64(w.ByWeekday[d]) / float64(maxDay) * 20)
		}
		lines = append(lines, pyfmt.MustValue(names[d], "<"+strconv.Itoa(width))+" "+bar(n, 20)+" "+pyfmt.MustValue(w.ByWeekday[d], ">6,"))
	}
	e.Fields = append(e.Fields, field(t("commands.stats_view.heatmap_day_title", lang), "```\n"+strings.Join(lines, "\n")+"\n```", false))

	if w.BestWeekday != nil {
		e.Fields = append(e.Fields, field(t("commands.stats_view.heatmap_peak_day_title", lang),
			t("commands.stats_view.heatmap_peak_value", lang, "label", locale.Weekday(*w.BestWeekday, lang), "count", w.ByWeekday[*w.BestWeekday]), true))
	}

	var hourSum, maxHour int64
	for _, c := range w.ByHour {
		hourSum += c
		maxHour = max(maxHour, c)
	}
	if hourSum > 0 {
		keys := []string{"commands.stats_view.timeblock_night", "commands.stats_view.timeblock_morning",
			"commands.stats_view.timeblock_afternoon", "commands.stats_view.timeblock_evening"}
		labels := make([]string, len(keys))
		bw := 0
		for i, k := range keys {
			labels[i] = t(k, lang)
			bw = max(bw, runeLen(labels[i]))
		}
		var hl []string
		for i, label := range labels {
			var total int64
			for h := i * 6; h < i*6+6; h++ {
				total += w.ByHour[h]
			}
			n := 0
			if maxHour > 0 {
				n = int(float64(total) / float64(maxHour*6) * 15)
			}
			hl = append(hl, pyfmt.MustValue(label, "<"+strconv.Itoa(bw))+" "+bar(n, 15)+" "+pyfmt.MustValue(total, ">5,"))
		}
		e.Fields = append(e.Fields, field(t("commands.stats_view.heatmap_time_title", lang), "```\n"+strings.Join(hl, "\n")+"\n```", false))
		if w.PeakHour != nil {
			ph := *w.PeakHour
			label := fmt.Sprintf("%02d:00-%02d:00", ph, (ph+1)%24)
			e.Fields = append(e.Fields, field(t("commands.stats_view.heatmap_peak_hour_title", lang),
				t("commands.stats_view.heatmap_peak_value", lang, "label", label, "count", w.ByHour[ph]), true))
		}
	}

	if w.Total >= 50 && w.ActiveDays >= 7 && w.Recommend != nil {
		r := w.Recommend
		e.Fields = append(e.Fields, field(t("commands.stats_view.heatmap_recommend_title", lang),
			t("commands.stats_view.heatmap_recommend_value", lang, "day", locale.Weekday(r[0], lang),
				"start", hourLabel(r[1]), "end", hourLabel(r[2]%24), "tz", tz), false))
		if q := w.Quiet; q != nil {
			e.Fields = append(e.Fields, field(t("commands.stats_view.heatmap_quiet_title", lang),
				t("commands.stats_view.heatmap_quiet_value", lang, "day", locale.Weekday(q[0], lang),
					"start", hourLabel(q[1]), "end", hourLabel(q[2]%24)), true))
		}
	}
	return e
}

func participationEmbed(seg db.ParticipationSegments, lurkers, ghosts []db.SegmentMember, returnsCount int64, returns []db.MemberReturn, lang string) discord.Embed {
	e := newEmbed(t("commands.stats_view.participation_title", lang), ColorDarkGrey)
	if seg.Lurkers == 0 && seg.Ghosts == 0 && len(returns) == 0 {
		e.Description = t("commands.stats_view.participation_no_data", lang)
		return e
	}
	e.Description = t("commands.stats_view.participation_desc", lang)
	name := func(m db.SegmentMember) string { return strOr(m.Nickname, m.Username) }

	var ll []string
	for _, m := range lurkers {
		var when string
		switch {
		case m.LastSeen != nil && *m.LastSeen == 0:
			when = t("commands.stats_view.participation_online_now", lang)
		case truthy(m.LastSeen):
			when = t("commands.stats_view.participation_last_seen", lang, "when", fmtTS(*m.LastSeen, "R", lang))
		default:
			when = t("commands.stats_view.participation_never_seen", lang)
		}
		ll = append(ll, t("commands.stats_view.participation_line", lang, "name", name(m), "detail", when))
	}
	if seg.Lurkers > int64(len(lurkers)) {
		ll = append(ll, t("commands.stats_view.participation_more", lang, "count", seg.Lurkers-int64(len(lurkers))))
	}
	value := t("commands.stats_view.participation_none", lang)
	if len(ll) > 0 {
		value = strings.Join(ll, "")
	}
	e.Fields = append(e.Fields, field(t("commands.stats_view.participation_lurkers_title", lang, "count", seg.Lurkers), value, false))

	var gl []string
	for _, m := range ghosts {
		when := t("commands.stats_view.participation_never_seen", lang)
		if truthy(m.LastSeen) {
			when = t("commands.stats_view.participation_last_seen", lang, "when", fmtTS(*m.LastSeen, "R", lang))
		}
		gl = append(gl, t("commands.stats_view.participation_line", lang, "name", name(m), "detail", when))
	}
	if seg.Ghosts > int64(len(ghosts)) {
		gl = append(gl, t("commands.stats_view.participation_more", lang, "count", seg.Ghosts-int64(len(ghosts))))
	}
	value = t("commands.stats_view.participation_none", lang)
	if len(gl) > 0 {
		value = strings.Join(gl, "")
	}
	e.Fields = append(e.Fields, field(t("commands.stats_view.participation_ghosts_title", lang, "count", seg.Ghosts), value, false))

	if len(returns) > 0 {
		var rl []string
		for _, r := range returns {
			rl = append(rl, t("commands.stats_view.returns_line", lang, "name", strOr(r.Nickname, r.Username),
				"days", r.AwaySeconds/86400, "returned", fmtTS(r.ReturnedAt, "R", lang)))
		}
		if returnsCount > int64(len(returns)) {
			rl = append(rl, t("commands.stats_view.returns_more", lang, "count", returnsCount-int64(len(returns))))
		}
		e.Fields = append(e.Fields, field(t("commands.stats_view.returns_title", lang), strings.Join(rl, ""), false))
	}
	return e
}

func lifecycleEmbed(seg db.LifecycleSegments, lang string) discord.Embed {
	e := newEmbed(t("commands.stats_view.lifecycle_title", lang), ColorBlurple)
	var total int64
	for _, n := range seg {
		total += n
	}
	if total == 0 {
		e.Description = t("commands.stats_view.lifecycle_no_data", lang)
		return e
	}
	var lines []string
	for _, bucket := range db.LifecycleOrder {
		lines = append(lines, t("commands.stats_view.lifecycle_line", lang,
			"label", t("commands.stats_view.lifecycle_"+bucket, lang), "count", seg[bucket]))
	}
	e.Description = t("commands.stats_view.lifecycle_desc", lang) + "\n\n" + strings.Join(lines, "\n")
	e.Footer = &discord.EmbedFooter{Text: t("commands.stats_view.lifecycle_footer", lang, "total", total)}
	return e
}

func healthEmbed(h *db.ServerHealth, history []db.HealthSnapshot, lang string) discord.Embed {
	e := newEmbed(t("commands.stats_view.health_title", lang), ColorTeal)
	if h == nil {
		e.Description = t("commands.stats_view.health_no_data", lang)
		return e
	}
	p := computeCommunityPulse(h)
	headline := t("commands.stats_view.pulse_headline", lang, "state", t("commands.stats_view.pulse_"+p.State, lang))
	if p.State == "collecting" {
		headline += "\n" + t("commands.stats_view.pulse_collecting_note", lang, "need", 60, "days", h.DaysOfData)
	} else if p.PostersDelta != nil && p.MessagesDelta != nil {
		headline += "\n" + t("commands.stats_view.pulse_detail", lang,
			"posters", formatPctArrow(*p.PostersDelta), "messages", formatPctArrow(*p.MessagesDelta))
	}
	e.Description = headline + "\n\n" + t("commands.stats_view.health_desc", lang)
	delta := func(cur, prev int64, ok bool) string {
		if !ok {
			return ""
		}
		return formatHealthDelta(cur, prev, lang)
	}

	week := []string{
		t("commands.stats_view.health_posters_line", lang, "cur", h.Posters7d, "delta", delta(h.Posters7d, h.PostersPrev7d, h.WeeklyComparable)),
		t("commands.stats_view.health_messages_line", lang, "cur", h.Messages7d, "delta", delta(h.Messages7d, h.MessagesPrev7d, h.WeeklyComparable)),
	}
	if !h.WeeklyComparable {
		week = append(week, t("commands.stats_view.health_no_compare", lang, "need", 14, "days", h.DaysOfData))
	}
	e.Fields = append(e.Fields, field(t("commands.stats_view.health_week_title", lang), strings.Join(week, "\n"), false))

	var breadth any = int64(0)
	if h.ActiveMembers > 0 {
		breadth = float64(h.Posters30d) / float64(h.ActiveMembers) * 100
	}
	e.Fields = append(e.Fields, field(t("commands.stats_view.health_breadth_title", lang),
		t("commands.stats_view.health_breadth_line", lang, "cur", h.Posters30d, "members", h.ActiveMembers, "pct", breadth,
			"delta", delta(h.Posters30d, h.PostersPrev30d, h.MonthlyComparable)), false))
	e.Fields = append(e.Fields, field(t("commands.stats_view.health_returns_title", lang),
		t("commands.stats_view.health_returns_line", lang, "cur", h.Returns30d,
			"delta", delta(h.Returns30d, h.ReturnsPrev30d, h.MonthlyComparable)), false))

	if len(history) >= 2 {
		var maxPosters int64
		for _, r := range history {
			maxPosters = max(maxPosters, r.Posters7d)
		}
		if maxPosters == 0 {
			maxPosters = 1
		}
		var lines []string
		for _, r := range history {
			label := time.Unix(r.Date, 0).UTC().Format("01-02")
			lines = append(lines, label+" "+bar(int(float64(r.Posters7d)/float64(maxPosters)*12), 12)+" "+
				pyfmt.MustValue(r.Posters7d, ">5,")+" │ "+pyfmt.MustValue(r.ActiveMembers, ">6,"))
		}
		e.Fields = append(e.Fields, field(t("commands.stats_view.health_trend_title", lang), "```\n"+strings.Join(lines, "\n")+"\n```", false))
	} else {
		e.Fields = append(e.Fields, field(t("commands.stats_view.health_trend_title", lang), t("commands.stats_view.health_trend_no_data", lang), false))
	}
	e.Footer = &discord.EmbedFooter{Text: t("commands.stats_view.health_footer", lang)}
	return e
}

// ---- LeaderboardView ----

func leaderboardPeriodName(days int, lang string) string {
	switch days {
	case 7:
		return t("commands.leaderboard.period_7", lang)
	case 30:
		return t("commands.leaderboard.period_30", lang)
	case 90:
		return t("commands.leaderboard.period_90", lang)
	case 0:
		return t("commands.leaderboard.period_all", lang)
	}
	return t("commands.leaderboard.period_other", lang, "days", days)
}

func (b *Bot) leaderboardEmbed(gid int64, days int, lang string) discord.Embed {
	e := newEmbed(t("commands.leaderboard.title", lang, "period", leaderboardPeriodName(days, lang)), ColorGold)
	board := b.DB.GetActivityLeaderboard(gid, days, 10)
	if len(board) == 0 {
		e.Description = t("commands.leaderboard.no_data", lang)
		return e
	}
	medals := []string{"🥇", "🥈", "🥉"}
	var lines []string
	for i, m := range board {
		medal := t("commands.leaderboard.rank", lang, "i", i+1)
		if i < 3 {
			medal = medals[i]
		}
		lines = append(lines, t("commands.leaderboard.line", lang, "medal", medal, "name", strOr(m.Nickname, m.Username), "count", m.TotalMessages))
	}
	e.Description = strings.Join(lines, "\n")
	e.Footer = &discord.EmbedFooter{Text: t("commands.leaderboard.footer", lang)}
	return e
}

// leaderboardView is LeaderboardView: a period select and a back button.
func (b *Bot) leaderboardView(guildID, ownerID snowflake.ID, lang string) (*View, discord.Embed) {
	gid := i64(guildID)
	v := b.newView(300*time.Second).ownerOnly(ownerID, lang)
	opt := func(key, value, emoji string, def bool) discord.StringSelectMenuOption {
		return discord.StringSelectMenuOption{Label: t(key, lang), Value: value,
			Emoji: &discord.ComponentEmoji{Name: emoji}, Default: def}
	}
	options := []discord.StringSelectMenuOption{
		opt("commands.leaderboard.period_7", "7", "📅", false),
		opt("commands.leaderboard.period_30", "30", "📅", true),
		opt("commands.leaderboard.period_90", "90", "📅", false),
		opt("commands.leaderboard.period_all", "0", "🌟", false),
	}
	v.Select(t("commands.leaderboard.select_placeholder", lang), options, 1, 1, 0, func(ix *Ix) {
		ix.DeferUpdate()
		days := 30
		if vals := ix.Values(); len(vals) > 0 {
			days, _ = strconv.Atoi(vals[0])
		}
		ix.EditOriginal(Msg{Embeds: []discord.Embed{b.leaderboardEmbed(gid, days, lang)}, View: v})
	})
	v.Button(t("commands.stats_view.btn_back", lang), discord.ButtonStyleSecondary, "", 1, func(ix *Ix) {
		ix.DeferUpdate()
		b.returnToOverview(ix, guildID, ownerID, lang)
	})
	return v, b.leaderboardEmbed(gid, 30, lang)
}
