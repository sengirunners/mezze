# zm plugin for Claude Code

Connects every Claude Code session on this machine to the local zm daemon.

| piece | runs | does |
| --- | --- | --- |
| `SessionStart` hook | `zm hook claude session-start` | registers the session, tells the agent its address, exports `ZM_AS` to its shell |
| `UserPromptSubmit` hook | `zm hook claude prompt` | marks the session `running`, activity `Thinking` (never the prompt); adds the `zm notice` line if mail is unread (never a mail body) |
| `PreToolUse` hook | `zm hook claude tool` | marks the session `running` with its activity (`Editing store.go`, `Searching`, Bash's own description); 5 s timeout |
| `Notification` hook | `zm hook claude notification` | `needs_human` on a permission prompt, else `idle` |
| `Stop` hook | `zm hook claude stop` | marks the session `idle` |
| `SessionEnd` hook | `zm hook claude session-end` | marks a session zm did not start `ended` (outcome ok) |
| MCP server `zm` | `zm mcp` | whoami, project, mail, work and session tools |

Hooks never block Claude Code: if the daemon is not running, or the repo is in no zm project, they print
nothing and exit 0.

The activity is shown to everyone who can see the session, teammates on a shared project included, so it never
holds a command, a prompt, a search pattern or file contents: Bash gives only the `description` Claude writes for
the command (else `Running a command`), file tools only the file's base name, web tools only the host, other tools
their name. `Stop` clears it (an idle session shows no activity).

## Install

You need `zm` on your `PATH` and its daemon running (`zm daemon --detach`). Then either use this plugin
(from a clone of the zm repo):

```
/plugin marketplace add ./integrations
/plugin install zm@zm
```

or let zm edit your user settings directly. It shows the exact diff first; `--yes` writes, with a backup
of every file it changes:

```
zm install claude
zm install claude --yes
```

Pick one. Both together run every hook twice. `zm uninstall claude --yes` removes what `zm install`
added and nothing else.

`hooks/hooks.json` is generated from the same code as `zm install claude`; a test keeps them in sync.
