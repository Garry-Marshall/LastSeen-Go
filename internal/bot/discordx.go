package bot

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// Small Discord helpers standing in for discord.py conveniences.

// shards is a snapshot of the shard manager's shards. Always use this
// instead of ranging over ShardManager.Shards() directly: that iterator
// holds the manager's lock (a plain, non-reentrant mutex) for the whole
// loop, so any shard-manager call inside the loop (SetPresenceForShard,
// Shard, ShardByGuildID...) deadlocks and blocks every later caller.
func (b *Bot) shards() []gateway.Gateway {
	return slices.Collect(b.Client.ShardManager.Shards())
}

// restStatus is the HTTP status of a failed REST call (0 when the error is
// not an HTTP response, e.g. a network failure).
func restStatus(err error) int {
	var rerr *rest.Error
	if errors.As(err, &rerr) && rerr.Response != nil {
		return rerr.Response.StatusCode
	}
	return 0
}

// isForbidden / isNotFound are discord.Forbidden / discord.NotFound.
func isForbidden(err error) bool { return restStatus(err) == 403 }
func isNotFound(err error) bool  { return restStatus(err) == 404 }

// guildChannel is guild.get_channel_or_thread: a cached channel or thread
// that belongs to the guild.
func (b *Bot) guildChannel(guildID, channelID snowflake.ID) (discord.GuildChannel, bool) {
	ch, ok := b.Client.Caches.Channel(channelID)
	if !ok || ch.GuildID() != guildID {
		return nil, false
	}
	return ch, true
}

// selfPermissionsIn is channel.permissions_for(guild.me); a thread uses its
// parent's permissions, like discord.py.
func (b *Bot) selfPermissionsIn(ch discord.GuildChannel) (discord.Permissions, bool) {
	self, ok := b.Client.Caches.SelfMember(ch.GuildID())
	if !ok {
		return 0, false
	}
	if th, ok := ch.(discord.GuildThread); ok {
		parent, ok := b.Client.Caches.Channel(*th.ParentID())
		if !ok {
			return 0, false
		}
		ch = parent
	}
	return b.Client.Caches.MemberPermissionsInChannel(ch, self), true
}

// selfCanSend is channel.permissions_for(guild.me).send_messages.
func (b *Bot) selfCanSend(ch discord.GuildChannel) bool {
	p, ok := b.selfPermissionsIn(ch)
	return ok && p.Has(discord.PermissionSendMessages)
}

// cachedMemberCtx describes a cached member for the permission helpers
// (member.guild_permissions and member.roles).
func (b *Bot) cachedMemberCtx(guildID snowflake.ID, m discord.Member) memberCtx {
	return memberCtx{
		GuildID:     guildID,
		Permissions: b.Client.Caches.MemberPermissions(m),
		Roles:       b.memberRoles(guildID, m.RoleIDs),
	}
}

// guildRoles is guild.roles: every cached role, sorted by position.
func (b *Bot) guildRoles(guildID snowflake.ID) []discord.Role {
	var roles []discord.Role
	for r := range b.Client.Caches.Roles(guildID) {
		roles = append(roles, r)
	}
	sortRoles(roles)
	return roles
}

// findUser is guild.get_member(id) or bot.get_user(id): the user from this
// guild's member cache, else from any guild the bot shares with them.
func (b *Bot) findUser(guildID, userID snowflake.ID) (discord.User, bool) {
	if m, ok := b.Client.Caches.Member(guildID, userID); ok {
		return m.User, true
	}
	for g := range b.Client.Caches.Guilds() {
		if m, ok := b.Client.Caches.Member(g.ID, userID); ok {
			return m.User, true
		}
	}
	return discord.User{}, false
}

// sendDM is user.send(): open (or reuse) the DM channel and post.
func (b *Bot) sendDM(userID snowflake.ID, m discord.MessageCreate) error {
	ch, err := b.Client.Rest.CreateDMChannel(userID)
	if err != nil {
		return err
	}
	_, err = b.Client.Rest.CreateMessage(ch.ID(), m)
	return err
}

// runeCut is Python's s[:n] (code points, not bytes).
func runeCut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// isASCIIDigits is str.isdigit() for the inputs the bot meets (ids, numbers).
func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// pyIntList is repr() of a Python list of ints: "[1, 2, 3]".
func pyIntList(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// chunk is chunk_list: consecutive slices of at most size items.
func chunk[T any](items []T, size int) [][]T {
	var out [][]T
	for i := 0; i < len(items); i += size {
		out = append(out, items[i:min(i+size, len(items))])
	}
	return out
}

func mentionUser(id int64) string    { return "<@" + strconv.FormatInt(id, 10) + ">" }
func mentionRole(id int64) string    { return "<@&" + strconv.FormatInt(id, 10) + ">" }
func mentionChannel(id int64) string { return "<#" + strconv.FormatInt(id, 10) + ">" }
