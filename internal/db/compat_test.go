package db

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestPythonCompat replays the operations a Python run applied (ops.json,
// written by the compat_py.py helper) on a fresh Go database, and runs the
// read queries on a copy of the Python-written database, writing the results
// to reads_go.json for comparison. Skipped unless LASTSEEN_COMPAT_DIR is set.
func TestPythonCompat(t *testing.T) {
	dir := os.Getenv("LASTSEEN_COMPAT_DIR")
	if dir == "" {
		t.Skip("LASTSEEN_COMPAT_DIR not set")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "ops.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ops [][]any
	if err := json.Unmarshal(raw, &ops); err != nil {
		t.Fatal(err)
	}
	defer func(f func() int64) { nowUTC = f }(nowUTC)

	goPath := filepath.Join(dir, "go.db")
	for _, s := range []string{"", "-wal", "-shm"} {
		os.Remove(goPath + s)
	}
	nowUTC = func() int64 { return int64(ops[0][1].(float64)) }
	d, err := Open(goPath, 4)
	if err != nil {
		t.Fatal(err)
	}
	applyOps(t, d, ops)
	d.Close()

	readPath := filepath.Join(dir, "pyread.db")
	copyFile(t, filepath.Join(dir, "py.db"), readPath)
	defer func() {
		for _, s := range []string{"", "-wal", "-shm"} {
			os.Remove(readPath + s)
		}
	}()
	var now int64
	for _, op := range ops {
		if op[0] == "set_now" {
			now = int64(op[1].(float64))
		}
	}
	nowUTC = func() int64 { return now }
	r, err := Open(readPath, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := json.MarshalIndent(compatReads(r), "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reads_go.json"), out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, src, dst string) {
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	o, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if _, err := io.Copy(o, in); err != nil {
		t.Fatal(err)
	}
}

func applyOps(t *testing.T, d *DB, ops [][]any) {
	i := func(v any) int64 { return int64(v.(float64)) }
	ip := func(v any) *int64 {
		if v == nil {
			return nil
		}
		n := i(v)
		return &n
	}
	sp := func(v any) *string {
		if v == nil {
			return nil
		}
		s := v.(string)
		return &s
	}
	strs := func(v any) []string {
		out := []string{}
		for _, x := range v.([]any) {
			out = append(out, x.(string))
		}
		return out
	}
	for _, op := range ops {
		a := op[1:]
		switch op[0] {
		case "set_now":
			n := i(a[0])
			nowUTC = func() int64 { return n }
		case "add_guild":
			d.AddGuild(i(a[0]), a[1].(string), int(i(a[2])))
		case "set_timezone":
			d.SetTimezone(i(a[0]), a[1].(string), a[2].(string))
		case "add_member":
			d.AddMember(i(a[0]), i(a[1]), a[2].(string), sp(a[3]), i(a[4]), strs(a[5]))
		case "update_last_seen":
			d.UpdateLastSeen(i(a[0]), i(a[1]), i(a[2]))
		case "inc_daily":
			d.IncrementMessageActivity(i(a[0]), i(a[1]), i(a[2]), i(a[3]))
		case "inc_hourly":
			d.IncrementMessageActivityHourly(i(a[0]), i(a[1]), i(a[2]), int(i(a[3])), i(a[4]))
		case "add_member_return":
			d.AddMemberReturn(i(a[0]), i(a[1]), i(a[2]), i(a[3]))
		case "role_change":
			d.RecordRoleChange(i(a[0]), i(a[1]), a[2].(string), a[3].(string))
		case "nickname":
			d.UpdateMemberNickname(i(a[0]), i(a[1]), sp(a[2]))
		case "set_inactive":
			d.SetMemberInactive(i(a[0]), i(a[1]))
		case "set_left":
			d.SetMemberLeftDate(i(a[0]), i(a[1]), ip(a[2]))
		case "add_watch":
			d.AddWatch(i(a[0]), a[1].(string), i(a[2]), a[3].(string), ip(a[4]), ip(a[5]), i(a[6]), a[7].(bool))
		case "health_snapshots":
			d.RecordHealthSnapshots()
		case "opt_out":
			d.AddOptedOutUser(i(a[0]))
		case "no_watch":
			d.AddNoWatchUser(i(a[0]))
		case "purge":
			d.PurgeUserData(i(a[0]))
		default:
			t.Fatalf("unknown op %v", op[0])
		}
	}
}

func compatReads(d *DB) map[string]any {
	out := map[string]any{}
	for _, g := range []int64{1001, 1002} {
		cfg := d.GetGuildConfig(g)
		tz := cfg.Timezone
		f := d.GetTrackedUserIDs(g)
		o := map[string]any{}
		o["guild_stats"] = d.GetGuildStats(g, nil)
		o["activity_stats"] = d.GetActivityStats(g, nil)
		o["participation"] = d.GetParticipationSegments(g, 30, nil)
		o["lifecycle"] = d.GetLifecycleSegments(g)
		o["lurkers"] = d.GetLurkers(g, 30, 10)
		o["ghosts"] = d.GetGhosts(g, 30, 0)
		o["snapshot"] = d.GetServerSnapshotStats(g)
		o["growth30"] = d.GetMemberGrowthStats(g, 30, nil)
		o["growth365"] = d.GetMemberGrowthStats(g, 365, nil)
		o["cohorts"] = d.GetRetentionCohorts(g, nil)
		o["funnel"] = d.GetActivationFunnel(g, nil)
		o["lifespan"] = d.GetDepartureLifespan(g, nil)
		o["health"] = d.GetServerHealth(g, nil)
		o["health_history"] = d.GetHealthHistory(g, 12, 7)
		o["leaderboard30"] = d.GetActivityLeaderboard(g, 30, 10)
		o["leaderboard0"] = d.GetActivityLeaderboard(g, 0, 10)
		byDay, _ := d.GetActivityByDay(g, 30, f, tz)
		o["by_day"] = byDay
		o["windows"] = d.GetServerActivityWindows(g, 30, tz, nil)
		o["msg_stats"] = d.GetGuildMessageActivityStats(g, 30, nil)
		o["new_members"] = d.GetNewMembersPeriod(g, 30)
		o["departed"] = d.GetDepartedMembersPeriod(g, 60)
		o["top_active"] = d.GetTopActiveUsersPeriod(g, 30, 10, nil)
		o["inactive"] = d.GetInactiveMembers(g, 10)
		o["find_x"] = d.FindMemberByName(g, "mr_x3")
		o["search"] = d.SearchMembersByName(g, "STRASSE", 25)
		o["search2"] = d.SearchMembersByName(g, "zo", 25)
		o["watches"] = d.GetGuildWatches(g)
		totals := map[string]any{}
		for k, v := range d.GetGuildActivityTotals(g, 30) {
			totals[strconv.FormatInt(k, 10)] = v
		}
		o["totals"] = totals
		users := map[string]any{}
		members := d.GetGuildMembers(g, false)
		if len(members) > 12 {
			members = members[:12]
		}
		for _, m := range members {
			u := m.UserID
			users[strconv.FormatInt(u, 10)] = map[string]any{
				"period":         d.GetMessageActivityPeriod(g, u, 30),
				"profile":        d.GetActivityProfile(g, u, 30, tz),
				"percentile":     d.GetActivityPercentile(g, u, 30),
				"journey":        d.GetMemberJourney(g, u),
				"trend":          d.GetMessageActivityTrend(g, u, 14),
				"whois":          d.GetWhoisActivityPanel(g, u, 30, tz),
				"days_with_data": d.GetMemberDaysWithData(g, u, 30),
				"role_history":   d.GetRoleHistory(g, u, 20),
			}
		}
		o["users"] = users
		out[strconv.FormatInt(g, 10)] = o
	}
	out["bot_stats"] = d.GetBotStatistics()
	out["opted_out"] = d.GetOptedOutUserIDs()
	out["no_watch"] = d.GetNoWatchUserIDs()
	return out
}
