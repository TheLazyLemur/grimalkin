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

## Safety and control

4. **Sandboxed shell.** A decorator over the shell tool that runs commands
   under `sandbox-exec` (macOS) or `bwrap` (Linux): writes limited to the cwd,
   network allow-listed. The biggest gap today, and a prerequisite for auto
   mode, since an unsandboxed shell can't be safely auto-approved.
5. **Checkpoints and rewind.** Snapshot the working tree before each turn (a
   shadow git ref, not the user's stash). `:rewind` restores files and
   conversation to any earlier message. With 3, a branch covers the
   filesystem as well as the chat.
6. **Hooks on core events.** Attach shell scripts to the events the core
   already emits (tool started, tool finished, …). A pre-tool hook can veto
   or rewrite a command. Covers linting, notifications and policy without
   growing the core.

## Agentic and tooling

7. **Diff-based edit tool with vimdiff review.** A file edit tool whose
   proposed changes render as a diff block. `]c`/`[c` jump between hunks,
   `do`/`dp` accept or reject each. Better than the model driving `sed`
   through the shell.
8. **Background jobs.** The shell has no timeout, so a dev server blocks
   everything. Let the model start processes in the background, show a jobs
   pane (`:jobs`), and give the model a tool to read a job's output tail or
   kill it. Subagents could reuse the same mechanism.
9. **Headless mode and replay.** A second inbound adapter,
   `grimalkin -p "prompt"`, emitting JSON events to stdout for scripts and
   CI. Pair it with a fake Model adapter that replays recorded sessions as
   regression tests for prompt and loop changes. Cheap, since the port
   already exists.
10. **Message queuing and steering.** Enter is ignored while a turn runs,
    and Ctrl-C is the only way in. Queuing: hold the message and send it as
    the next turn. Steering: hand it to the model mid-turn, appended after
    the round's tool results (never edited into earlier messages, so the
    cache and thinking stay valid). Queuing is TUI-only; steering touches
    the core loop.
11. **Web search and web fetch.** Today the model can only reach the web
    through `curl` in the shell, with no search at all. Add both as
    grimalkin's own tools, backed by Ollama's web search and web fetch
    APIs, so they work the same whichever provider runs the model.
12. **Classify tool.** Jev answers one fixed question today, inside
    search. A `classify` tool takes a list of paths and a yes/no question
    the model writes, reads the files itself, scores them through search's
    existing worker pool and returns only the hits: 200 classifications in
    one call, with none of the file contents in context. Needs a cap on
    paths per call, since it sends whatever it is pointed at.
13. **Code mode, in Lua.** A tool whose input is a Lua script, run in an
    embedded VM with no file, network or OS libraries. Every other tool is
    a function on `tools`. Only what the script returns reaches the model,
    so a chain where one step's output decides the next (classify files,
    hand each hit to a subagent, classify the reports) takes one turn
    instead of three, with the intermediate results kept out of context.
    Needs a timeout and an output cap. Worth building once the model is
    seen doing such chains by hand with 12 and subagents. After
    [pi's code mode](https://pi.dev/docs/latest/codemode), which uses
    JavaScript.
14. **Search chunks by function and paragraph.** Search cuts files into
    8 KB runs of whole lines, so a range can start mid-function and one
    excerpt can hold four unrelated ones. Cut at declarations instead, and
    at paragraphs in prose: ranges name a whole function, Jev scores an
    excerpt about one thing, and the second pass matters less. Merge small
    neighbours up to a minimum size, and keep the line splitter for giant
    functions and for file types without a parser. Each chunk also gets
    its symbol's name, so the repo map could tag a file from its list of
    declarations rather than its first 8 KB. Start with `go/parser` for
    `.go` files, which needs no dependency, and measure with
    `TestSearchShortlistRecall` and the benchmark. Tree-sitter would cover
    the other languages, at the cost of cgo and a grammar compiled in per
    language.

## Also worth considering

- Session persistence and resume (a JSONL transcript adapter).
- Token and cost meter in the status bar, with `:compact`.
- MCP client.

## Suggested order

4, then 1 and 5. The sandbox unblocks auto mode; the ex line and checkpoints
are what most of the rest builds on.
