package main

import "github.com/spf13/cobra"

// helpGroups sorts the top-level verbs of zm --help into sections, most
// used first. A verb in none of them lists under "Additional Commands".
// Each module still registers its own verb in init(); groupCommands runs
// once every init has, from main (and tests).
var helpGroups = []struct {
	id, title string
	verbs     []string
}{
	{"mail", "Mail:", []string{"send", "inbox", "reply", "ack", "thread", "mail", "sent", "notice"}},
	{"work", "Work items and leases:", []string{"work", "claim", "release", "who", "lease", "leases", "inflight", "issues", "tracker", "github"}},
	{"asks", "Asks (a person decides):", []string{"ask", "people", "approver", "permit", "receipt", "measures"}},
	{"sessions", "Agent sessions:", []string{"ls", "attach", "kill", "session", "owed", "ssh"}},
	{"run", "Run work elsewhere:", []string{"run", "exec", "tasks", "job", "pool", "sandbox", "host", "lend", "target", "runners", "ci"}},
	{"setup", "Setup and teams:", []string{"init", "install", "uninstall", "doctor", "status", "whoami", "project", "area", "join", "leave", "invite", "remotes", "grant", "grants", "revoke", "identity"}},
}

func groupCommands(root *cobra.Command) {
	if len(root.Groups()) > 0 {
		return // once
	}
	byName := map[string]*cobra.Command{}
	for _, c := range root.Commands() {
		byName[c.Name()] = c
	}
	for _, g := range helpGroups {
		root.AddGroup(&cobra.Group{ID: g.id, Title: g.title})
		for _, v := range g.verbs {
			if c := byName[v]; c != nil {
				c.GroupID = g.id
			}
		}
	}
}
