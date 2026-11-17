# Claude one-shot task context

At task start, after the actual worktree cwd is resolved, zm creates
`scratch/<session>/task-context/plugin`. Claude loads it natively through
`--plugin-dir`; accepted SKILL.md and resource bytes are not rewritten or
appended to a prompt. Native CLAUDE.md and its imports use `--add-dir` for the
repository root through the cwd and
`CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1`. Outside a repository, only the
cwd is included. No repository settings are enabled.

The fixed plugin namespace is **zm-task-repo**:
`/zm-task-repo:<skill-directory>`. Instructions remain native/live; skill
resources are frozen for this task. Source changes do not change the copied
skill. A new continuation takes a new snapshot from its resolved worktree.
Daemon restart/re-adoption uses the existing holder and snapshot. Failed
launches remove scratch through the existing undo path; successful snapshots
remain bounded scratch artifacts with the session, including after cancellation,
and are removed with that scratch directory. They are not regenerated on adoption.

## Supported input and refusal

Only `.claude/skills/<name>/` from root-to-cwd ancestors is copied.
Names are lower-case letters/digits/hyphens, at most 64 bytes; duplicates across
ancestors refuse launch. Native name shadowing is not emulated. SKILL.md must
have UTF-8 YAML frontmatter with an explicit nonempty description; a name,
when present, must equal its directory. Top-level fields supported:

- name, description, argument-hint
- disable-model-invocation, user-invocable (literal booleans)
- allowed-tools, disallowed-tools, paths
- model, context (`fork`), agent (general-purpose, Explore, Plan), shell
- license, compatibility

Values must be plain or quoted scalars. Description/argument-hint also accept
indented text blocks using `|`, `>`, `|-`, `>-`. Numeric/boolean-looking
text (including case-insensitive YAML boolean/null spellings) must be quoted.
Plain scalars cannot begin with reserved YAML indicators or end with a colon.
Header control characters, tabs, Unicode line separators and BOMs are refused;
double-quoted decoded controls are also refused. Mapping colons require a
following space. Collections, aliases, anchors, tags, merge keys, duplicate
keys, nested mappings, custom agents and unknown fields refuse launch.
This is a conservative admissible-input check, not a replacement YAML loader.
Claude parses and invokes the unchanged accepted skill.

Every source path is opened through an `os.Root` capability. Symlinks
(including contained ones), special files, replaced files/directories and
oversized inputs refuse launch; nonblocking/no-follow file opens avoid FIFO
replacement hangs. Limits: 32 skills, 1,024 resource entries including
directories, 1 MiB per file, 8 MiB total, 8 nested resource levels, 32 cwd
ancestors. Copy ordering and the source-content SHA256 audit are deterministic.
The generated plugin manifest contains only name/version. Configuration
components inside a selected skill refuse launch before being read:
`.claude`, `.claude-plugin`, `.mcp.json`, `.lsp.json`, `settings.json`,
`settings.local.json`, and `hooks`.

## Isolation and compatibility limits

Empty `--setting-sources`, `disableAllHooks`, strict zm-only MCP, and the
existing authentication environment are preserved. The additional
`disableSkillShellExecution` setting disables inline/fenced shell
preprocessing. Hooks and shell preprocessing are separate execution paths.
A nonempty plugin requires verifiable Claude Code **2.1.318 or newer**;
unversioned/wrapped runtimes fail closed rather than risk an ignored setting.
The parent performs filesystem-only preparation. A private managed launcher
waits for a one-shot pipe grant after durable task/holder/group records exist.
It then probes the version with a 3-second deadline, 4-KiB capture and null
stdin, in the recorded task group, and execs the CLI without consuming its
prompt. A cold task needs no warmed version cache. Unsupported or failed
probes produce plain stderr and a nonzero task exit, never synthetic provider
events. Task version metadata is cached-only. Existing sandbox requests put
the gate outside their wrapper and use the same sandbox policy for the probe
and final command; existing wrapper/PID semantics remain unchanged.
No config directory, credentials, provider endpoint, OAuth or HOME is changed.

This is not full native project customization parity. Legacy
`.claude/commands`, lazy descendant discovery, existing repository plugins,
custom agents, hooks, MCP/LSP, rich YAML, and native collision/override rules
are not imported. Arbitrary forked-skill lifecycle and live reload parity
remain unproved, so this is not yet full native parity. Ordinary agent tools still have their existing
permissions: plugin paths are not a filesystem sandbox.

Synthetic validation uses an OS network sandbox and invented stored OAuth
with a loopback fake responder, never real provider calls. Basic native
instruction/import/body/resource loading and all eight project credential/
provider override cases must pass using the actual packager before shipping.
