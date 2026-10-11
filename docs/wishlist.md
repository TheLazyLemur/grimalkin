# Wishlist

Ideas for grimalkin beyond what's already planned. Nothing here is committed
to.

**Planned:** subagents, auto mode. Auto mode has no spec yet: the shell
already runs every command without asking, so what it adds is still to be
defined.

The three tiers are in order. Foundation is what a coding agent needs before
the rest is worth building; Identity is what makes grimalkin its own; Later
waits for both.

## Foundation

1. **Sandboxed shell.** A decorator over the shell tool that runs commands
   under `sandbox-exec` (macOS) or `bwrap` (Linux): writes limited to the cwd,
   network allow-listed. The biggest gap today, and a prerequisite for auto
   mode, since an unsandboxed shell can't be safely auto-approved.
2. **File edit tool.** A tool that replaces an exact string in a file, or
   writes a whole file, so the model stops driving `sed` through the shell.
   Its result is a diff, which the transcript renders as a diff block.
3. **Session persistence and resume.** A JSONL transcript adapter: every
   event is appended to a file per session, and a session can be resumed at
   startup. Everything under Projects depends on it.
4. **Context compaction and a token meter.** A token and cost meter in the
   status bar, and `:compact` (`/compact` until 7 exists), which summarises
   the conversation so far and carries on from the summary. Without it a
   long session ends at the context limit.
5. **Project instructions file.** Read `AGENTS.md` from the project root into
   the system prompt at startup. Nothing reads one today.
6. **Headless mode and replay.** A second inbound adapter,
   `grimalkin -p "prompt"`, emitting JSON events to stdout for scripts and
   CI. Pair it with a fake Model adapter that replays recorded sessions as
   regression tests for prompt and loop changes. Cheap, since the port
   already exists.

## Identity

7. **Ex command line (`:`).** `:model`, `:clear`, `:w session.md`. `:r !cmd`
   runs a shell command yourself and injects its output as context; `:!cmd`
   runs one without the model seeing it. One entry point for most features
   below, instead of a new keybinding each.
8. **Quickfix list.** The model fills a list of `file:line` entries (grep
   hits, test failures, review findings). `:cn`/`:cp` step through them; `gf`
   opens the entry in `$EDITOR` at that line.
9. **Vimdiff review of edits.** A proposed edit from 2 waits for review:
   `]c`/`[c` jump between hunks, `do`/`dp` accept or reject each.
10. **Checkpoints and rewind.** Snapshot the working tree before each turn (a
    shadow git ref, not the user's stash). `:rewind` restores files and
    conversation to any earlier message. With 15, a branch covers the
    filesystem as well as the chat.
11. **Message queuing and steering.** Enter is ignored while a turn runs,
    and Ctrl-C is the only way in. Queuing: hold the message and send it as
    the next turn. Steering: hand it to the model mid-turn, appended after
    the round's tool results (never edited into earlier messages, so the
    cache and thinking stay valid). Queuing is TUI-only; steering touches
    the core loop.
12. **Background jobs.** The shell has no timeout, so a dev server blocks
    everything. Let the model start processes in the background, show a jobs
    pane (`:jobs`), and give the model a tool to read a job's output tail or
    kill it. Subagents could reuse the same mechanism.
13. **Hooks on core events.** Attach shell scripts to the events the core
    already emits (tool started, tool finished, …). A pre-tool hook can veto
    or rewrite a command. Covers linting, notifications and policy without
    growing the core.

## Later

14. **MCP client.**
15. **Conversation undo tree.** Editing and resending an earlier message
    starts a branch instead of overwriting history. `g-`/`g+` move between
    branches, like vim's undotree. Fits the event-sourced transcript.
16. **Classify tool.** Jev answers one fixed question today, inside
    search. A `classify` tool takes a list of paths and a yes/no question
    the model writes, reads the files itself, scores them through search's
    existing worker pool and returns only the hits: 200 classifications in
    one call, with none of the file contents in context. Needs a cap on
    paths per call, since it sends whatever it is pointed at.
17. **Code mode, in Lua.** A tool whose input is a Lua script, run in an
    embedded VM with no file, network or OS libraries. Every other tool is
    a function on `tools`. Only what the script returns reaches the model,
    so a chain where one step's output decides the next (classify files,
    hand each hit to a subagent, classify the reports) takes one turn
    instead of three, with the intermediate results kept out of context.
    Needs a timeout and an output cap. Worth building once the model is
    seen doing such chains by hand with 16 and subagents. After
    [pi's code mode](https://pi.dev/docs/latest/codemode), which uses
    JavaScript.
18. **Search chunks by function and paragraph.** Search cuts files into
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

### Projects

One long-lived goal worked across many sessions and repos, after Claude
Code's Projects, without the cloud execution. A separate product surface
from the single-session agent above: it sits on 1, 3, 6 and subagents, and
should stay unplanned until those exist.

Undecided: whether a thread outlives the terminal. If it does, grimalkin
needs a daemon. If it does not, threads and routines only run while the TUI
is open.

19. **Project store.** A project is a name, a goal and a list of repos or
    folders, kept outside any one repo. It also holds the project's
    settings: model and effort for the coordinator and for threads
    separately, how many threads run at once, how often to report, and
    whether to show a plan before starting.
20. **Durable threads.** A thread is a full session on its own branch, in
    its own git worktree. It outlives the turn that started it and can be
    reopened, steered (11) or stopped. Subagents as planned are children of
    one turn and end with it.
21. **Coordinator conversation.** One ongoing conversation per project. It
    explores the repos, proposes what to work on, splits the goal into
    tasks and starts a thread for each, then offers next steps to pick
    from.
22. **Threads view.** A list of the project's threads and their state, with
    the ones waiting on the user first.
23. **Pull request tools.** A thread opens a PR when a change is ready, or
    when asked. It keeps watching the PR, pushing fixes when CI fails and
    answering review comments, and merges only when asked. A second view
    lists each PR with its status and next step. Must work on both GitHub
    and Bitbucket Cloud: one code host port with an adapter per platform,
    picked from the repo's git remote. `review-assist`
    (`~/Programming/tools/review-assist`) is the worked example: the
    `pr.CodeHost` port, a GitHub adapter over `gh`, a Bitbucket adapter
    over REST, and remote detection in `gitrepo/remote.go`. Its port reads,
    comments on and merges PRs but does not create them, and only the
    GitHub adapter reads checks, so creating a PR and reading Bitbucket
    Pipelines status are new work.
24. **Project memory.** Preferences the user states are saved and loaded
    into every later thread. A plain file, so it can be edited by hand.
25. **Library.** A project folder every thread can read. The user adds
    mock-ups and specs; threads write artifacts there, such as a summary
    report with before-and-after metrics.
26. **Routines.** A scheduled headless run (6) that checks something and
    opens a thread when it finds a problem. `cron` or `launchd` does the
    scheduling.
