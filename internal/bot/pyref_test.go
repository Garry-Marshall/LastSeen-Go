package bot

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/disgoorg/disgo/discord"

	"lastseen/internal/db"
	"lastseen/internal/locale"
)

// Golden tests against testdata/pyref.json, generated from the Python bot by
// tools/gen_pyref.py: the port must lay out every view (labels, emoji,
// styles, rows, disabled state) and parse/format values exactly like the
// original.

type pyref struct {
	Views []struct {
		Name string             `json:"name"`
		Rows [][]map[string]any `json:"rows"`
	} `json:"views"`
	ParseDuration []struct {
		In  string   `json:"in"`
		Out *float64 `json:"out"`
	} `json:"parse_duration"`
	FormatDuration []struct {
		In  int64  `json:"in"`
		Out string `json:"out"`
	} `json:"format_duration"`
	FormatAway []struct {
		In  int64  `json:"in"`
		Out string `json:"out"`
	} `json:"format_away"`
	EscapeMarkdown []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	} `json:"escape_markdown"`
	DiffMemberState []struct {
		Cache    map[string]bool    `json:"cache"`
		Rows     map[string][2]*int `json:"rows"`
		Online   []int64            `json:"online"`
		Offline  []int64            `json:"offline"`
		Rejoined []int64            `json:"rejoined"`
		New      []int64            `json:"new"`
		Departed []int64            `json:"departed"`
	} `json:"diff_member_state"`
}

var (
	refOnce sync.Once
	ref     pyref
)

func loadRef(t *testing.T) *pyref {
	t.Helper()
	refOnce.Do(func() {
		locale.Load("../../locales")
		raw, err := os.ReadFile("testdata/pyref.json")
		if err != nil {
			t.Fatalf("read pyref.json: %v", err)
		}
		if err := json.Unmarshal(raw, &ref); err != nil {
			t.Fatalf("parse pyref.json: %v", err)
		}
	})
	return &ref
}

// layoutOf normalises a view's components like gen_pyref.py's layout().
func layoutOf(v *View) [][]map[string]any {
	var rows [][]map[string]any
	for _, lc := range v.components() {
		row := []map[string]any{}
		for _, c := range lc.(discord.ActionRowComponent).Components {
			switch c := c.(type) {
			case discord.ButtonComponent:
				emoji := ""
				if c.Emoji != nil {
					emoji = c.Emoji.Name
				}
				row = append(row, map[string]any{"type": 2.0, "style": float64(c.Style), "label": c.Label,
					"emoji": emoji, "disabled": c.Disabled})
			case discord.StringSelectMenuComponent:
				minV := 1
				if c.MinValues != nil {
					minV = *c.MinValues
				}
				opts := []any{}
				for _, o := range c.Options {
					opts = append(opts, map[string]any{"label": o.Label, "value": o.Value, "default": o.Default})
				}
				row = append(row, map[string]any{"type": 3.0, "placeholder": c.Placeholder,
					"min": float64(minV), "max": float64(c.MaxValues), "options": opts})
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func testGuildConfig(lang string, roleRequired bool) *db.GuildConfig {
	ch, freq := int64(123), "weekly"
	return &db.GuildConfig{Language: lang, UserRoleRequired: roleRequired, InactiveDays: 14,
		BotAdminRoleName: "Mods", UserRoleName: "LastSeen User", Timezone: "Europe/Brussels",
		NotificationChannelID: &ch, ReportFrequency: &freq}
}

// buildView builds the Go view named like the Python reference entry.
func buildView(b *Bot, name string) *View {
	parts := strings.Split(name, "/")
	lang := parts[1]
	cfg := testGuildConfig(lang, false)
	switch parts[0] {
	case "config":
		return b.configView(1, testGuildConfig(lang, parts[2] == "1"))
	case "quick_setup":
		v, _ := b.quickSetupView(1, cfg)
		return v
	case "disable_reports":
		return b.disableReportsConfirmView(1, cfg)
	case "language":
		return b.languageSelectView(1, cfg)
	case "forgetme":
		return b.forgetMeView(5, lang)
	case "user_stats":
		return b.userStatsView(1, 5, lang)
	case "journey":
		return b.journeyView(1, 2, "x", 5, lang)
	case "pagination":
		pages, _ := strconv.Atoi(parts[2])
		var export []exportMember
		if parts[3] == "True" {
			export = []exportMember{{Username: "a"}}
		}
		return b.newPaginationView(make([]discord.Embed, pages), 5, lang, export)
	}
	return nil
}

func TestViewsMatchPython(t *testing.T) {
	r := loadRef(t)
	b := &Bot{ui: newUIRegistry()}
	for _, want := range r.Views {
		t.Run(want.Name, func(t *testing.T) {
			v := buildView(b, want.Name)
			if v == nil {
				t.Fatalf("no Go view for %s", want.Name)
			}
			got := layoutOf(v)
			// Round-trip through JSON so both sides use the same types.
			var norm [][]map[string]any
			raw, _ := json.Marshal(got)
			_ = json.Unmarshal(raw, &norm)
			if !reflect.DeepEqual(norm, want.Rows) {
				gj, _ := json.MarshalIndent(norm, "", " ")
				wj, _ := json.MarshalIndent(want.Rows, "", " ")
				t.Errorf("layout differs\n got: %s\nwant: %s", gj, wj)
			}
		})
	}
}

func TestParseDurationMatchesPython(t *testing.T) {
	for _, c := range loadRef(t).ParseDuration {
		got, ok := parseDuration(c.In)
		switch {
		case c.Out == nil:
			if ok {
				t.Errorf("parseDuration(%q) = %d, want rejected", c.In, got)
			}
		case *c.Out > math.MaxInt64:
			if !ok || got != math.MaxInt64 {
				t.Errorf("parseDuration(%q) = %d, %v; want out-of-range MaxInt64", c.In, got, ok)
			}
		default:
			if !ok || float64(got) != *c.Out {
				t.Errorf("parseDuration(%q) = %d, %v; want %v", c.In, got, ok, *c.Out)
			}
		}
	}
}

func TestFormatDurationMatchesPython(t *testing.T) {
	r := loadRef(t)
	for _, c := range r.FormatDuration {
		if got := formatDuration(c.In); got != c.Out {
			t.Errorf("formatDuration(%d) = %q, want %q", c.In, got, c.Out)
		}
	}
	for _, c := range r.FormatAway {
		if got := formatAway(c.In); got != c.Out {
			t.Errorf("formatAway(%d) = %q, want %q", c.In, got, c.Out)
		}
	}
}

func TestEscapeMarkdownMatchesPython(t *testing.T) {
	for _, c := range loadRef(t).EscapeMarkdown {
		if got := escapeMarkdown(c.In); got != c.Out {
			t.Errorf("escapeMarkdown(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
}

func TestDiffMemberStateMatchesPython(t *testing.T) {
	for i, c := range loadRef(t).DiffMemberState {
		cache := map[int64]bool{}
		for k, v := range c.Cache {
			id, _ := strconv.ParseInt(k, 10, 64)
			cache[id] = v
		}
		rows := map[int64]db.PresenceRow{}
		for k, v := range c.Rows {
			id, _ := strconv.ParseInt(k, 10, 64)
			var ls *int64
			if v[0] != nil {
				x := int64(*v[0])
				ls = &x
			}
			rows[id] = db.PresenceRow{LastSeen: ls, IsActive: v[1] != nil && *v[1] != 0}
		}
		d := diffMemberState(cache, rows)
		for _, p := range []struct {
			name      string
			got, want []int64
		}{
			{"online", d.Online, c.Online}, {"offline", d.Offline, c.Offline},
			{"rejoined", d.Rejoined, c.Rejoined}, {"new", d.New, c.New}, {"departed", d.Departed, c.Departed},
		} {
			if !slices.Equal(p.got, p.want) && !(len(p.got) == 0 && len(p.want) == 0) {
				t.Errorf("case %d %s: got %v, want %v", i, p.name, p.got, p.want)
			}
		}
	}
}

func TestParseIntInput(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int64
		isOK bool
	}{
		{"42", 42, true}, {" 7 ", 7, true}, {"+3", 3, true}, {"-3", -3, true}, {"1_000", 1000, true},
		{"", 0, false}, {"abc", 0, false}, {"1__0", 0, false}, {"_1", 0, false}, {"1_", 0, false},
		{"+-1", 0, false}, {"1.5", 0, false}, {"99999999999999999999999", -1, true},
	} {
		n, ok := parseIntInput(c.in)
		if ok != c.isOK || (ok && n != c.n) {
			t.Errorf("parseIntInput(%q) = %d, %v; want %d, %v", c.in, n, ok, c.n, c.isOK)
		}
	}
}
