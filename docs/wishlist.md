# Wishlist

Ideas for grimalkin beyond what's already planned. Nothing here is committed
to.

**Planned:** subagents, auto mode.

## Vim-native UI

1. **Ex command line (`:`).** `:model`, `:clear`, `:w session.md`. `:r !cmd`
   runs a shell command yourself and injects its output as context; `:!cmd`
   runs one without the model seeing it. One entry point for most features
   below, instead of a new keybinding each.
2. **Quickfix list.** The model fills a list of `file:line` entries (grep
   hits, test failures, review findings). `:cn`/`:cp` step through them; `gf`
   opens the entry in `$EDITOR` at that line.
3. **Conversation undo tree.** Editing and resending an earlier message starts
   a branch instead of overwriting history. `g-`/`g+` move between branches,
   like vim's undotree. Fits the event-sourced transcript.
4. **Folds and registers in VIEW.** `za` folds a tool output block, `zM`
   collapses all tool output. Named registers (`"ay`) persist yanks across the
   session and can be pasted into the input as context.

## Safety and control

5. **Sandboxed shell.** A decorator over the shell tool that runs commands
   under `sandbox-exec` (macOS) or `bwrap` (Linux): writes limited to the cwd,
   network allow-listed. The biggest gap today, and a prerequisite for auto
   mode, since an unsandboxed shell can't be safely auto-approved.
6. **Checkpoints and rewind.** Snapshot the working tree before each turn (a
   shadow git ref, not the user's stash). `:rewind` restores files and
   conversation to any earlier message. With 3, a branch covers the
   filesystem as well as the chat.
7. **Hooks on core events.** Attach shell scripts to the events the core
   already emits (tool started, tool finished, …). A pre-tool hook can veto
   or rewrite a command. Covers linting, notifications and policy without
   growing the core.

## Agentic and tooling

8. **Diff-based edit tool with vimdiff review.** A file edit tool whose
   proposed changes render as a diff block. `]c`/`[c` jump between hunks,
   `do`/`dp` accept or reject each. Better than the model driving `sed`
   through the shell.
9. **Background jobs.** The shell has no timeout, so a dev server blocks
   everything. Let the model start processes in the background, show a jobs
   pane (`:jobs`), and give the model a tool to read a job's output tail or
   kill it. Subagents could reuse the same mechanism.
10. **Headless mode and replay.** A second inbound adapter,
    `grimalkin -p "prompt"`, emitting JSON events to stdout for scripts and
    CI. Pair it with a fake Model adapter that replays recorded sessions as
    regression tests for prompt and loop changes. Cheap, since the port
    already exists.
11. **Message queuing and steering.** Enter is ignored while a turn runs,
    and Ctrl-C is the only way in. Queuing: hold the message and send it as
    the next turn. Steering: hand it to the model mid-turn, appended after
    the round's tool results (never edited into earlier messages, so the
    cache and thinking stay valid). Queuing is TUI-only; steering touches
    the core loop.

## Also worth considering

- Session persistence and resume (a JSONL transcript adapter).
- Token and cost meter in the status bar, with `:compact`.
- MCP client.

## Suggested order

5, then 1 and 6. The sandbox unblocks auto mode; the ex line and checkpoints
are what most of the rest builds on.
