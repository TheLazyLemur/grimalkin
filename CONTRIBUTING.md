# Contributing

## Architecture

Ports and adapters: a pure domain core defines the ports it needs, adapters
implement or drive them, and one composition root wires them together.

```
main.go                   composition root: loads config, wires adapters
internal/config/          loads and validates ~/.config/grimalkin/config.json
internal/core/            conversation, turn loop, tool rules, events,
                          the skill and agent tools, ports (Model, Tool,
                          Skills, Transcript, History)
internal/adapters/
  anthropic/              outbound: Model port over anthropic-sdk-go
  tools/                  outbound: tools (tools.Shell, tools.Search and
                          its Jev client, tools.WebSearch and
                          tools.WebFetch and their Ollama client)
  skills/                 outbound: Skills port over SKILL.md directories,
                          rendering arguments, variables and !`cmd`
  clipboard/              outbound: tui.Clipboard via pbcopy
  acp/                    inbound: ACP agent over stdio (`grimalkin acp`);
                          drives the core via acp.Agent, is its Transcript,
                          History and Gate
  tui/                    inbound: Ultraviolet UI, editor, navigation,
                          markdown; drives the core via tui.Agent
scripts/check-imports.sh  enforces the import rules
```

**Import rules** (checked by `make check`):

- The core imports nothing from the application.
- An adapter imports only the core, never another adapter.
- Only `main.go` sees concrete adapters.

**Ports.** The core's outbound ports live in `internal/core/ports.go`. The TUI
defines the ports it consumes (`tui.Agent`, satisfied by `*core.Agent`, and
`tui.Clipboard`, and `tui.RepoMap`, the repo map's state for the status bar
and its `/index` command, satisfied by `*tools.Search`), since Go interfaces
belong with their users.

**Events.** The core reports a turn to `Transcript` as events
(`internal/core/events.go`): reply started, reply text, tool started, tool
finished, skill loaded, steer delivered, turn discarded. `Show` takes the number of the agent
the event belongs to beside the event, the parent being 0; the core emits
through `Agent.show`, which passes it. A child's lifecycle has its own events,
each naming the child: waiting, started, progress, finished, failed,
cancelled. The TUI decides how each looks: it draws a child's lifecycle as
one status line in the parent's transcript, updated in place, which is all
an `agent` call shows there. `tui.Store` keeps a transcript per agent, its
buffer: the parent's from the start, a child's from its first lifecycle
event, for the session. A turn event goes to its agent's buffer. The model
shows one buffer at a time (`model.show`) and keeps the cursor and scroll of
the others as they were left. `:b N` on the ex command line and `gf` on a
status line switch buffers; `:ls` draws its list over the transcript and
writes it to no buffer. The status bar and `:ls` write a child's model
with the label `main.go` hands `tui.Run` for it, keyed by the `provider/model`
name its lifecycle events carry; a name with no label panics.

**Tools.** A tool implements `core.Tool`: a spec, a `Summary` of a call as a
command line, and `Run`, which returns output or an error and must honour its
context. Tools never touch the screen. The core looks tools up by name, answers
unknown names with an error, and caps each result at 30 KB (keeping the tail).

A response's tool calls run at the same time and their results go back in
call order, so a tool's methods and `Transcript.Show` are called from several
goroutines at once. Skill tool calls are the exception: they run first and
one at a time, because each is deduplicated against the ones before it.
Cancelling the turn cancels every running call. An agent with a `Gate` set
(optional; the TUI sets none, children never have one) asks it after a call's
`ToolStarted` and before the tool runs; a call it refuses doesn't run, and the
model gets the reason as an error result. The gate may block until the turn's
context is cancelled, which ends the call as a cancelled run does. A panic in one call cancels
the others and is raised again from `RunTurn`, with the call's stack in its
message.

The core has two tools of its own, which are not in any list: `skill`, offered
when the model may load at least one skill, and `agent`, offered when
`Agent.SubagentTools` is set (see Skills and Subagents below). A tool in
either list may not take those names.

To add a tool, add a type to `internal/adapters/tools` and list it in
`toolCatalogue` in `main.go`. That gives it to the parent only. Children get
the list `main.go` sets as `Agent.SubagentTools`, for now the shell alone; a
child also has `skill`, because it shares the parent's `Skills`. `main.go`
sets `Agent.ModelName` to `provider/model` from the config selection, and
`Agent.SubagentModels` to an adapter per entry of the config's `subagents`
block, named the same way and in its order (`subagentModels`). `newModel`
maps a provider's wire to its adapter; an entry on a wire it doesn't know is
a startup error naming the entry.

**ACP.** `acp.Server` (`internal/adapters/acp`) is the second driver of the
core, beside the TUI, and implements `core.Transcript`, `core.History` and
`core.Gate` itself: `main.go` wires it as the `core.Agent`'s transcript,
history and gate, then calls `Serve(newAgent, stdin, stdout)`; `newAgent`
builds the agent at `session/new` or `session/load`, once the client's tools
are known. The server speaks on the SDK's `Connection` and dispatches the
requests itself (`rpc.go`), because an `AgentSideConnection` keeps its
connection private and the `mcp/message` requests below can't be sent
without it. One session per process. A `session/prompt` starts a turn with `StartTurn`,
so a `session/cancel` that arrives straight away still reaches it, and runs it
on the request's goroutine and answers when it returns: `end_turn`, `cancelled` if
`session/cancel` came in meanwhile, or an error carrying the reason of a
`TurnDiscarded`. The adapter enforces one turn at a time, since the core has
no busy guard. `ReplyText` becomes `agent_message_chunk`, `ToolStarted` a
`tool_call` (pending; the tool's name goes in `_meta.toolName`, its kind is
mapped from the name in `toolKind`, so a new tool that isn't `other` is added
there) and `ToolFinished` a `tool_call_update`; other events are not shown.
The agent has the TUI's tools and skills, never the `agent` tool, and
`Allow` is the gate: it sends `session/request_permission` and blocks on the
answer, returning once the turn's context is done. Only the client selecting
`allow` runs a call.

**Models.** `acp.New` takes `acp.Models`: every model the client may pick
(`provider/model` and its efforts), the default and its effort, and `Build`,
which builds one at an effort. `main.go` (`acpModels`) lists the config's
`Selection.Models` and builds through `newModel`, getting the provider's key
from `Selection.Keys`, which runs a key command the first time its
provider's key is asked for and keeps the key for the process. A headless
load runs only the selected provider's at startup. `session/new` and
`session/load` answer with the `model` and `thought_level` options
(`models.go`); `session/set_config_option` builds the new choice without
`s.mu` held, saves it, then calls `Agent.SetModel`. The effort carried to
another model follows `switchedEffort`, config's subagent rule but taking
the new model's highest when the old one had no levels; it lives in the
adapter as config can't be imported. `session/close` cancels and waits for
a running turn, deletes the file if `turns` is 0, and clears the session.

**Steering.** `_session/steering` (`{sessionId, prompt}`; `_meta` is ignored)
calls `Agent.Steer` with the prompt's text and answers `{"outcome":"injected"}`,
or `{"outcome":"promptRequired","reason":"noRunningTurn"}` on `core.ErrNoTurn`;
empty text or another session is invalid params. `initialize` advertises it as
`_meta.steering.supported` beside `agentCapabilities`. `SteerDelivered` is
saved as a `steer` display record in the turn's order and is not sent live,
since the client has the message.

**Client tools** (`mcp.go`) come from `type: "acp"` entries of `mcpServers`;
other transports are skipped with a note on stderr. For each, before
`session/new` or `session/load` answers, the server sends `mcp/connect`
(`{"acpId": <the entry's id>}`), then MCP `initialize`, `notifications/initialized`
and `tools/list` as `mcp/message` (`{connectionId, method, params}`), sent with
`sdk.SendRequest` on the connection. Each listed tool becomes a `core.Tool`
that runs as `tools/call`; its text blocks are the result, and `isError` makes
it an error. A tool whose name isn't API-safe, or is the name of the agent's
own tools (the names `acp.New` is given) or of another client tool, fails the
session start. The SDK's client side doesn't dispatch an inbound `mcp/message`
(only `mcp/connect`), so the tests route those lines past it (`serveMcp`).

The server is also the agent's `History`, and saves the session
(`store.go`) as one JSONL file named by its id under
`$XDG_STATE_HOME/grimalkin/sessions/` (dir 0700, file 0600), created at
`session/new` and only ever appended to. Each record carries its turn's
number and is either a display record, what the client was shown (the
prompt when the turn starts; then, when the turn is kept, its reply text and
tool calls in the order they were shown), or the turn's messages from
`History.Keep`. A turn with no messages record was discarded or never
finished. Display kinds are `prompt`, `reply`, `steer` (`text`), `tool_start`
(`tool_id`, `name`, `summary`, `input`) and `tool_end` (`tool_id`, `text`,
`failed`). A discarded turn that had steers is saved too, for them alone:
`Keep` writes its `steer` records and its messages record (the steers as one
user message) with `"discarded":true`, and replay shows only that turn's
steers. A last line that isn't JSON is a write the process died in:
`readSession` cuts it off the file and loads the rest; a bad line anywhere
else fails the load. A model record (`{"model":{"name","effort"}}`, no turn) is appended when the
client picks a model or effort; `session/load` restores the last one. `session/load` takes only an id of the shape
`session/new` makes, seeds the agent with every messages record, replays
the display records of kept turns as `user_message_chunk`,
`agent_message_chunk`, `tool_call` and `tool_call_update` (a `steer` record as
a `user_message_chunk`), and numbers new turns after the file's last. The
first failed write is logged and makes every later prompt answer an error. Tests drive the adapter
with the SDK's client side over `io.Pipe`. Spec: `docs/specs/acp.md`.

**Search.** `tools.Search` lists files with `rg --files` (so `.gitignore`
holds), filters and keyword-scores them in Go, and sends the best excerpt of
each of the best files to Jev's `/v1/systemone` as one Noul each. Keyword
scoring is BM25-like (rare terms weigh more, repeats saturate), adds a term's
rarity among paths when the path holds it, and discounts tests and docs.
Shortlist recall against labelled questions (`example/benchmark.json`, or
your own for other repos, kept out of this one) is measured offline by
`TestSearchShortlistRecall` (see its comment); no file missed by
the shortlist can be ranked by Jev. The model's `glob` is matched in Go, not
passed to rg, because an rg `--glob` overrides `.gitignore`. If Jev fails,
the tool returns the keyword matches and the reason rather than an error, and
never echoes Jev's error bodies. Tests run against a fake Jev on `httptest`.

After ranking, a second pass scores up to 40 more excerpts of the
shortlisted files (those with the most keywords) and moves each file to its
best excerpt, so a large file's range points at the part that answers the
query. A file whose first excerpt scored low can rise this way. If the
second pass fails or runs out of time, the first ranking stands and the
output says so.

The repo map (`search_map.go`, behind `"map": true`) is a prototype. It is
built once per session in a goroutine. In a git
repository the first search starts it, or `main.go` does with
`"mapAtStartup": true`; elsewhere only `/index` does, which the TUI handles
itself and never sends to the model. `/index` also rebuilds a map that
failed, and builds a ready one from scratch. Each file's tags come from one Jev request (a role Choice and a
concept Choice over up to 120 words harvested from paths and file contents,
leaving out words in more than two fifths of either). A Choice answer is
rejected unless its pick and its distribution's keys are options that were
sent, since both are printed for the model. A build in which most files fail
is failed; one that runs out of time keeps what it tagged. A search's query
gets a concept Choice too, within five seconds, and files are scored by the
product of the two distributions. Up to 15 of the 60 shortlist places go to
the map's picks, the rest going back to the keywords, and up to five of
those picks that Jev scores at least 0.5 are listed even below the top ten.

The map is stored in `.grimalkin/map.json` (`search_map_store.go`): the
vocabulary, and each file's tags with a hash of the first chunk they were
made from. A build reuses the tags whose hash still matches and the stored
vocabulary, since tags made against different vocabularies can't be
compared, and sends only the rest. It saves every 30 seconds and at the end,
so a build that is quit, fails or runs out of time is resumed by the next.
The file is repository content, so a clone can bring one: it is ignored
unless its version and model match and every role, concept and vocabulary
word is one a build could have written. Raise `searchMapVersion` when the
roles or questions change.

**Web.** `tools.WebSearch` and `tools.WebFetch` (`web.go`) share one
`tools.WebProvider`, the seam for swapping the service behind them. The
config's `web` block is keyed by provider name; `config` validates the names,
picks the first alphabetically, runs only its key command and writes the
startup line. To add a provider: implement `WebProvider` in
`internal/adapters/tools` (as `web_ollama.go` does, since an adapter may not
import another), add its name to `webProviders` in `internal/config` and its
constructor to `webProviders` in `main.go`. A provider whose service lacks
search or fetch supplies the missing one itself. The tools bound their own
output from the front, because the core's cap keeps the tail, and a
provider's errors never echo a response body, since the model reads them.

**Skills.** The `Skills` port lists skills and renders one; the adapter does
all file and shell I/O. The core owns the rest: the catalog in the system
prompt, the `skill` tool (only when the model may load at least one skill),
`/name args` in a prompt, and dedup. Dedup searches the message history for
identical content, so a discarded turn takes its skills with it. Skill
content is exempt from the 30 KB tool result cap; the adapter caps each
command's output instead. See `docs/specs/skills.md`.

**Subagents.** The core offers the `agent` tool when `Agent.SubagentTools`
is set (`ModelName` names the model in the status line). A call claims a
child in call order, before the calls run, so numbers follow the order the
model wrote them and a second call for the same child is answered "busy". A
child is an `Agent` with its own number and history, the parent's
transcript and skills, the model the call's `model` names (the parent's by
default; the parameter is offered only when `Agent.SubagentModels` lists
others, as an enum of the parent's `ModelName` and then theirs, and a
resumed child keeps the one it started on), the parent's system prompt plus a subagent note, and
the subagent tool list, so it is never offered `agent`. Its turn runs under
the parent's tool call context: cancelling the parent cancels it.
`Agent.CancelChild(n)` cancels child n alone, running or waiting: its turn is
discarded, its call is answered `[agent N]` and "cancelled by user" as an
error, and the parent's turn and its other children go on.
`Agent.StartChildTurn(n, prompt)` claims child n for a turn the user starts
on it, or returns why it can't (unknown, busy), and hands back the function
that runs it: the same run as a call's, slot and lifecycle events included,
but outside the parent's turn, so only `CancelChild` cancels it. At most five children run at once
(`maxRunningChildren`): a call requests its slot when it claims its child,
so slots go out in call order, and a child with none reports waiting until
one is released; cancelled while waiting, it ends cancelled without having
started. The call's
result is `[agent N]`, a newline and the text streamed by the child's last
response, capped below the number; a discarded turn gives the reason the
same way, as an error. A call with `fork` starts a fork instead: a copy of the parent's history up
to the response being handled, with the parent's own system prompt, tool
specs and model, so the request reads the parent's prompt cache. Its first
message answers every call of that response with a placeholder result and
then gives the task (a user `Message` with both `ToolResults` and `Text`,
sent results first); a fork whose first turn is discarded gets the
placeholders again on its next. A fork is offered `agent` like the parent
and is answered "subagents can't start subagents". Children live in the
parent for the session. See
`docs/specs/subagents.md`.

**Messages.** Assistant message content is opaque to the core: the provider
stores the SDK's own `MessageParam` as JSON (`json.RawMessage`, thinking
blocks and signatures intact) and decodes it again for each request; content
it can't decode fails the call. A `Message` is plain data, so a conversation
saved with `encoding/json` goes back to the provider as it was. Each
assistant message names the model that wrote it (`Message.Model`, the agent's
`ModelName`, which is required), and each request names the model it goes to
(`ModelRequest.Model`). The provider drops thinking blocks from messages whose
`Model` differs, an empty one (saved before messages named it) included,
since it can't verify another model's signatures.
`Agent.SetModel(model, name)` switches the parent's `Model` and `ModelName`
from the next `StartTurn` on; it may be called while a turn runs, which ends
on the model it started with.

**Steering.** `Agent.Steer(text)` holds a user message for the running turn
(from `StartTurn` until it ends; the TUI never steers, and a child can't be
steered) and returns `ErrNoTurn` when there is none, in one step against the
turn ending, so a steer is either refused or delivered. The turn takes held
steers with a tool round's results, or, when they came during the call that
would end it, as one more round's user message; each is shown as
`SteerDelivered` when taken. A discarded turn keeps its steers, taken or
still held, as one user message at the end of the history; the next prompt
follows it as a user message of its own, and the two are joined into one
when the model is sent the conversation.

**History.** `Agent.History` (optional; the TUI sets none, and children
never have one) is given each kept turn's messages, in order, when the turn
ends, and a discarded turn's steers, as that one user message, after its
`TurnDiscarded`. Nothing else of a discarded turn is passed, so what it holds
is always the whole conversation. `Agent.Seed` starts an agent from such
messages before its first turn, and refuses any where an assistant message
doesn't follow a user message or has no content, or a user message that
follows a user message has tool results.

## Development

```sh
make build   # bin/grimalkin
make test    # import check, go vet, go test ./...
make clean
```

Tests live beside the code. The core is tested against port mocks; the
Anthropic adapter against a local `httptest` server streaming SSE.
