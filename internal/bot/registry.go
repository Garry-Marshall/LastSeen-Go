package bot

import (
	_ "embed"
	"encoding/json"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
)

// commandsJSON is the slash command definitions registered with Discord:
// names, descriptions, options and contexts. It is what the Python bot
// registered, so the commands look exactly the same to users.
//
//go:embed commands.json
var commandsJSON []byte

// syncCommands overwrites the global commands; returns how many Discord has.
func (b *Bot) syncCommands() (int, error) {
	var synced []json.RawMessage
	err := b.Client.Rest.Do(rest.SetGlobalCommands.Compile(nil, b.Client.ApplicationID), json.RawMessage(commandsJSON), &synced)
	if err != nil {
		return 0, err
	}
	return len(synced), nil
}

type autocompleteFunc func(ix *Ix, current string) []discord.AutocompleteChoice

// registerCommands wires every command path to its handler and cooldown.
func (b *Bot) registerCommands() {
	b.commands = map[string]commandSpec{}
	b.autocompletes = map[string]autocompleteFunc{}
	cmd := func(path string, cooldown time.Duration, perGuild bool, fn func(ix *Ix)) {
		b.commands[path] = commandSpec{handler: fn, cooldown: cooldown, perGuild: perGuild}
	}
	sec := func(n float64) time.Duration { return time.Duration(n * float64(time.Second)) }

	// User commands
	cmd("whois", sec(5), false, b.cmdWhois)
	cmd("lastseen", sec(5), false, func(ix *Ix) { b.lastseenImpl(ix, "lastseen") })
	cmd("seen", sec(5), false, func(ix *Ix) { b.lastseenImpl(ix, "seen") })
	cmd("role-history", sec(5), false, b.cmdRoleHistory)
	cmd("inactive", sec(10), false, b.cmdInactive)
	cmd("chat-history", sec(5), false, b.cmdChatHistory)
	cmd("mystats", sec(5), false, b.cmdMyStats)
	cmd("forgetme", sec(60), false, b.cmdForgetMe)
	cmd("optin", sec(60), false, b.cmdOptIn)
	cmd("no-watch", sec(10), false, b.cmdNoWatch)
	cmd("about", 0, false, b.cmdAbout)
	cmd("search", 0, false, b.cmdSearch)
	cmd("user-stats", 0, false, b.cmdUserStats)
	for _, name := range []string{"whois", "lastseen", "seen", "role-history", "chat-history"} {
		b.autocompletes[name] = b.userAutocomplete
	}

	// Admin
	cmd("config", sec(30), true, b.cmdConfig)
	cmd("help", 0, false, b.cmdHelp)
	b.autocompletes["help"] = b.helpAutocomplete

	// Watches
	cmd("watch online", 0, false, b.watch.cmdOnline)
	cmd("watch offline", 0, false, b.watch.cmdOffline)
	cmd("watch list", 0, false, b.watch.cmdList)
	cmd("watch remove", 0, false, b.watch.cmdRemove)
	b.autocompletes["watch remove"] = b.watch.removeAutocomplete
}
