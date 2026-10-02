# Skills

## Problem Statement

Agent Skills (agentskills.io) package task instructions as a directory with a
`SKILL.md`. Claude Code treats a skill as a template it renders on
activation (arguments, variables, injected command output), not a file the
model reads. grimalkin has only a shell, so a skill could at best be `cat`ed
with its placeholders unexpanded.

## Solution

Load skills from directories listed in the config, show the model a catalog
in its system prompt, and activate a skill through a dedicated `skill` tool or
a `/name args` prompt. Activation renders the skill like Claude Code does.

Features that need machinery grimalkin lacks are out: `allowed-tools` and
`disallowed-tools` and `Skill()` rules (permissions), `context: fork` and
`agent` (subagents), per-turn `model` and `effort`, `hooks`, re-attaching
skills after compaction, trust checks on project skills, live reloading of
the catalog, stacked `/a /b` invocation and `/` autocomplete.

## Implementation Decisions

- **Discovery is opt-in.** `skillDirs` in `config.json`, a list of
  directories; nothing is scanned by default. Each holds skill directories
  one level down. `~` expands to home, relative paths resolve against the
  working directory. Missing directories are skipped silently (a
  project-relative entry won't exist in most repos); a path that exists but
  isn't a directory is a startup error. An empty entry is a config error.
- **Precedence is list order.** The first skill with a name wins; shadowed
  skills are reported as startup info lines.
- **Lenient parsing,** following the agentskills.io client guide and Claude
  Code. Frontmatter is optional. The directory names the skill, as in Claude
  Code; a frontmatter `name` unlike it is a warning and otherwise ignored. A
  name off the spec is a warning; one containing whitespace is skipped (it
  can't be typed). `description` (plus
  `when_to_use`, capped at 1,536 characters) defaults to the body's first
  non-empty line; none at all skips the skill. YAML failing on an unquoted
  `: ` in a value is retried quoted; other YAML errors skip the skill.
- **Catalog** in the system prompt: `<available_skills>` with name and
  description, XML-escaped, for skills without `disable-model-invocation`.
  With none, there is no catalog and no tool.
- **`skill` tool:** `{name, arguments?}`, `name` an enum of the model's
  skills. The core rejects other names.
- **User invocation:** a prompt `/name args` where `name` is a skill without
  `user-invocable: false` is replaced by the rendered skill. Anything else
  starting with `/` (a path, an unknown name) is sent as typed. A failed
  render discards the turn without calling the model.
- **Rendering** reads `SKILL.md` afresh, drops the frontmatter, then in one
  pass over the original body: finds `` !`cmd` `` (at line start or after
  whitespace) and ` ```! ` blocks, expands arguments then
  `${GRIMALKIN_SKILL_DIR}` and `${GRIMALKIN_PROJECT_DIR}` in text and
  commands, and replaces each command with its output. Argument values and
  command output are never scanned for further placeholders or commands.
  Argument rules are Claude Code's: `$ARGUMENTS` as typed; `$ARGUMENTS[N]`
  and `$N` from shell-style splitting, left as written past the end; declared
  `$name` mapped by position, empty past the end; `\` escapes a placeholder;
  unused arguments appended as `ARGUMENTS: …`. No `CLAUDE_*` aliases and no
  session ID.
- **Injected commands** run like the shell tool (`/bin/sh -c`, working
  directory, closed stdin, process group killed on cancel), with stderr merged
  into stdout and a 2-minute timeout. Non-zero exit aborts the activation,
  except exit 1 from `grep`, `rg`, `egrep`, `fgrep`, `find`, `diff`, `test`,
  `[`, `git diff` and `git grep`. Output past 30 KB keeps its head.
- **Wrapping:** `<skill_content name="…">` with the body, the skill
  directory and a `<skill_resources>` list of bundled files (hidden ones
  skipped, at most 50), not read.
- **Dedup** in the core: if the rendered content is already in the history
  (a user message or a successful tool result, including earlier results in
  the same response), the model gets a short "already loaded" note instead.
  The history is the only record, so a discarded turn's skills don't count.
- **Skill content is not truncated** by the core's 30 KB tool result cap,
  since cutting it would drop instructions.

## Testing Decisions

- Core, against a mock `Skills`: catalog filtering and escaping, the tool's
  enum, model and user activation, rejection of user-only skills, render
  errors, dedup within and across responses, dedup after a discarded turn,
  slash prompts that aren't skills, and the tool name clash.
- Adapter, against temp directories: precedence and shadowing, lenient
  parsing cases, argument expansion cases, variables, injected commands
  (inline, fenced, not mid-word, merged stderr, working directory, no
  rescanning, exit-1 carve-out, failures, cancellation), wrapping and fresh
  reads.
- Config: `skillDirs` carried through in order; an empty entry rejected.
