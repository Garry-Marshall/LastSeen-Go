package bot

import (
	"strconv"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// Views shared by several commands.

// exportMember is one row of the pagination view's CSV export.
type exportMember struct {
	Username string
	Nickname string
	UserID   int64
	LastSeen *int64
	JoinDate *int64
	Roles    []string
}

// sanitizeCSVValue strips newlines and neutralises spreadsheet formula
// injection (cells starting with =, +, -, @ or a tab get a leading quote).
func sanitizeCSVValue(v string) string {
	v = strings.NewReplacer("\n", " ", "\r", " ").Replace(v)
	if v != "" && strings.ContainsRune("=+-@\t", rune(v[0])) {
		v = "'" + v
	}
	return v
}

// csvQuoteAll writes CSV with every field quoted and CRLF line endings.
func csvQuoteAll(rows [][]string) string {
	var sb strings.Builder
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('"')
			sb.WriteString(strings.ReplaceAll(cell, `"`, `""`))
			sb.WriteByte('"')
		}
		sb.WriteString("\r\n")
	}
	return sb.String()
}

// newPaginationView is PaginationView: first/prev/next/last buttons and an
// optional CSV export, usable only by its owner.
func (b *Bot) newPaginationView(embeds []discord.Embed, ownerID snowflake.ID, lang string, export []exportMember) *View {
	v := b.newView(180*time.Second).ownerOnly(ownerID, lang)
	page := 0
	var first, prev, next, last *Item
	update := func(ix *Ix) {
		first.Disabled = page == 0
		prev.Disabled = page == 0
		next.Disabled = page == len(embeds)-1
		last.Disabled = page == len(embeds)-1
		ix.Update(Msg{Embeds: []discord.Embed{embeds[page]}, View: v})
	}
	first = v.Button("⏮️", discord.ButtonStyleSecondary, "", -1, func(ix *Ix) {
		page = 0
		update(ix)
	})
	prev = v.Button("◀️", discord.ButtonStylePrimary, "", -1, func(ix *Ix) {
		page = max(0, page-1)
		update(ix)
	})
	next = v.Button("▶️", discord.ButtonStylePrimary, "", -1, func(ix *Ix) {
		page = min(len(embeds)-1, page+1)
		update(ix)
	})
	last = v.Button("⏭️", discord.ButtonStyleSecondary, "", -1, func(ix *Ix) {
		page = len(embeds) - 1
		update(ix)
	})
	exportBtn := v.Button("📄 Export CSV", discord.ButtonStyleSuccess, "", -1, func(ix *Ix) {
		ix.DeferUpdate()
		rows := [][]string{{"Username", "Nickname", "User ID", "Last Seen", "Joined", "Roles"}}
		for _, m := range export {
			lastSeen, joined := "", ""
			if m.LastSeen != nil && *m.LastSeen != 0 {
				lastSeen = time.Unix(*m.LastSeen, 0).UTC().Format("2006-01-02 15:04") + " UTC"
			}
			if m.JoinDate != nil && *m.JoinDate != 0 {
				joined = time.Unix(*m.JoinDate, 0).UTC().Format("2006-01-02")
			}
			rows = append(rows, []string{
				sanitizeCSVValue(m.Username),
				sanitizeCSVValue(m.Nickname),
				strconv.FormatInt(m.UserID, 10),
				lastSeen,
				joined,
				sanitizeCSVValue(strings.Join(m.Roles, ", ")),
			})
		}
		name := "inactive_members_" + time.Now().UTC().Format("20060102_150405") + ".csv"
		ix.Followup(Msg{
			Content:   t("commands.search.export_success", lang, "count", len(export), "format", "CSV"),
			Files:     []*discord.File{discord.NewFile(name, "", strings.NewReader(csvQuoteAll(rows)))},
			Ephemeral: true,
		})
	})
	if len(embeds) == 1 {
		first.Disabled, prev.Disabled, next.Disabled, last.Disabled = true, true, true, true
	}
	if len(export) == 0 {
		exportBtn.Hidden = true
	}
	return v
}
