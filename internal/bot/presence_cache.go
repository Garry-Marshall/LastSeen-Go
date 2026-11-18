package bot

import (
	"iter"
	"sync"

	"github.com/disgoorg/disgo/cache"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// statusCache is disgo's presence cache, stored compactly: only the status
// of members who are not offline (activities and client status are never
// kept). An offline presence removes the entry, so a member without one is
// offline.
type statusCache struct {
	mu sync.RWMutex
	m  map[snowflake.ID]map[snowflake.ID]discord.OnlineStatus
}

var _ cache.GroupedCache[discord.Presence] = (*statusCache)(nil)

func newStatusCache() *statusCache {
	return &statusCache{m: map[snowflake.ID]map[snowflake.ID]discord.OnlineStatus{}}
}

func presenceOf(guildID, userID snowflake.ID, s discord.OnlineStatus) discord.Presence {
	return discord.Presence{PresenceUser: discord.PresenceUser{ID: userID}, GuildID: guildID, Status: s}
}

func isOffline(s discord.OnlineStatus) bool {
	return s == "" || s == discord.OnlineStatusOffline || s == discord.OnlineStatusInvisible
}

// Online reports whether the member is online (online, idle or dnd).
func (c *statusCache) Online(guildID, userID snowflake.ID) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.m[guildID][userID]
	return ok
}

// Status is the member's status (offline when not cached).
func (c *statusCache) Status(guildID, userID snowflake.ID) discord.OnlineStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.m[guildID][userID]; ok {
		return s
	}
	return discord.OnlineStatusOffline
}

func (c *statusCache) Get(groupID, id snowflake.ID) (discord.Presence, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.m[groupID][id]
	if !ok {
		return discord.Presence{}, false
	}
	return presenceOf(groupID, id, s), true
}

func (c *statusCache) Put(groupID, id snowflake.ID, p discord.Presence) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if isOffline(p.Status) {
		if g := c.m[groupID]; g != nil {
			delete(g, id)
		}
		return
	}
	g := c.m[groupID]
	if g == nil {
		g = map[snowflake.ID]discord.OnlineStatus{}
		c.m[groupID] = g
	}
	g[id] = p.Status
}

func (c *statusCache) Remove(groupID, id snowflake.ID) (discord.Presence, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.m[groupID][id]
	if ok {
		delete(c.m[groupID], id)
	}
	return presenceOf(groupID, id, s), ok
}

func (c *statusCache) GroupRemove(groupID snowflake.ID) {
	c.mu.Lock()
	delete(c.m, groupID)
	c.mu.Unlock()
}

func (c *statusCache) RemoveIf(f cache.GroupedFilterFunc[discord.Presence]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for g, users := range c.m {
		for u, s := range users {
			if f(g, presenceOf(g, u, s)) {
				delete(users, u)
			}
		}
	}
}

func (c *statusCache) GroupRemoveIf(groupID snowflake.ID, f cache.GroupedFilterFunc[discord.Presence]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for u, s := range c.m[groupID] {
		if f(groupID, presenceOf(groupID, u, s)) {
			delete(c.m[groupID], u)
		}
	}
}

func (c *statusCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, g := range c.m {
		n += len(g)
	}
	return n
}

func (c *statusCache) GroupLen(groupID snowflake.ID) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m[groupID])
}

func (c *statusCache) All() iter.Seq2[snowflake.ID, discord.Presence] {
	return func(yield func(snowflake.ID, discord.Presence) bool) {
		c.mu.RLock()
		var all []discord.Presence
		for g, users := range c.m {
			for u, s := range users {
				all = append(all, presenceOf(g, u, s))
			}
		}
		c.mu.RUnlock()
		for _, p := range all {
			if !yield(p.GuildID, p) {
				return
			}
		}
	}
}

func (c *statusCache) GroupAll(groupID snowflake.ID) iter.Seq[discord.Presence] {
	return func(yield func(discord.Presence) bool) {
		c.mu.RLock()
		var all []discord.Presence
		for u, s := range c.m[groupID] {
			all = append(all, presenceOf(groupID, u, s))
		}
		c.mu.RUnlock()
		for _, p := range all {
			if !yield(p) {
				return
			}
		}
	}
}
