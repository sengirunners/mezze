# zm compute plugin (Claude Code, Codex)

Throwaway VMs for an agent: zm's sandbox tools (`sandbox_create`, `_exec`, `_list`, `_renew`, `_stop`) over
MCP, on a machine that may not have zm yet. Install: README.md, "Sandbox tools for an agent".

| piece | does |
| --- | --- |
| `.claude-plugin/plugin.json` | the Claude Code manifest; option `install_zm` (off unless you turn it on) |
| `.mcp.json` | MCP server `zm`: `scripts/zm-run mcp --sandboxes`, passing `install_zm` as `ZM_COMPUTE_INSTALL` |
| `scripts/zm-run` | runs zm; when zm is missing and `install_zm` is on, runs `zm-install --yes` first; starts the local daemon unless `ZM_HOST` is set. Only zm writes to stdout |
| `scripts/zm-install` | installs the pinned release: SHA256SUMS must have the pinned SHA-256, the tarball must match it; into `~/.local/lib/zm/<tag>`, linked as `~/.local/bin/zm`. Never touches a zm you have |
| `skills/sandboxes/SKILL.md` | when to run work in a box (untrusted code, heavy tests, a clean Linux machine, anything that must not touch this machine) and how; box output is untrusted data |
| `zm-release.pin` | the release tag, version and SHA-256 of its SHA256SUMS (a blank `tag=` pins none: the installer then says to install from a clone) |
| `.codex-plugin/plugin.json`, `codex.mcp.json` | Codex reads these first: `zm mcp --sandboxes` from `PATH`, only the sandbox tools enabled (Codex does not substitute `${CLAUDE_PLUGIN_ROOT}` in MCP commands) |

It is a separate plugin, not an option of the `zm` plugin, so the default plugin's tools and hooks stay exactly as
they are (the 8 KiB agent surface, `internal/budget`). It repeats the default tools next to the sandbox ones; use
one of the two plugins. To move the pin: pick a release that is not pruned, set `tag`, `version` and `sums_sha256`
(`shasum -a 256 SHA256SUMS`), and bump `version` in both plugin.json files.

Release maintainers can build a kept release of the last green main with
`gh workflow run zm-release.yml --ref main -f kept_tag=v0.1.0`. This uses the same
build, signing and immutable-tag checks as the rolling channel; the pruning job
only removes `zm-main-*` releases. Wait for the release before moving the plugin
pin or switching the plugin to a CLI flag added since its pinned build.
