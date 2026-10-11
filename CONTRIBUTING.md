# Contributing

## Architecture

Ports and adapters: a pure domain core defines the ports it needs, adapters
implement or drive them, and one composition root wires them together.

```
main.go                   composition root: loads config, wires adapters
internal/config/          loads and validates ~/.config/grimalkin/config.json
internal/core/            conversation, turn loop, tool rules, events,
                          the skill and agent tools, ports (Model, Tool,
                          Skills, Transcript)
internal/adapters/
  anthropic/              outbound: Model port over anthropic-sdk-go
  tools/                  outbound: tools (tools.Shell, tools.Search and
                          its Jev client, tools.WebSearch and
                          tools.WebFetch and their Ollama client)
  skills/                 outbound: Skills port over SKILL.md directories,
                          rendering arguments, variables and !`cmd`
  clipboard/              outbound: tui.Clipboard via pbcopy
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
finished, skill loaded, turn discarded. `Show` takes the number of the agent
the event belongs to beside the event, the parent being 0; the core emits
through `Agent.show`, which passes it. A child's lifecycle has its own events,
each naming the child: waiting, started, progress, finished, failed,
cancelled. The TUI decides how each looks: it draws a child's lifecycle as
one status line in the parent's transcript, updated in place, which is all
an `agent` call shows there, and for now shows only agent 0's turn events.

**Tools.** A tool implements `core.Tool`: a spec, a `Summary` of a call as a
command line, and `Run`, which returns output or an error and must honour its
context. Tools never touch the screen. The core looks tools up by name, answers
unknown names with an error, and caps each result at 30 KB (keeping the tail).

A response's tool calls run at the same time and their results go back in
call order, so a tool's methods and `Transcript.Show` are called from several
goroutines at once. Skill tool calls are the exception: they run first and
one at a time, because each is deduplicated against the ones before it.
Cancelling the turn cancels every running call. A panic in one call cancels
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
sets `Agent.ModelName` to `provider/model` from the config selection.

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
is set (`ModelName` is then required, for the status line). A call claims a
child in call order, before the calls run, so numbers follow the order the
model wrote them and a second call for the same child is answered "busy". A
child is an `Agent` with its own number and history, the parent's model,
transcript and skills, the parent's system prompt plus a subagent note, and
the subagent tool list, so it is never offered `agent`. Its turn runs under
the parent's tool call context: cancelling the parent cancels it. At most five children run at once
(`maxRunningChildren`): a call requests its slot when it claims its child,
so slots go out in call order, and a child with none reports waiting until
one is released; cancelled while waiting, it ends cancelled without having
started. The call's
result is `[agent N]`, a newline and the text streamed by the child's last
response, capped below the number; a discarded turn gives the reason the
same way, as an error. Children live in the parent for the session. See
`docs/specs/subagents.md`.

**Messages.** Assistant message content is opaque to the core: the provider
stores the SDK's own `MessageParam` (thinking blocks and signatures intact)
and gets it back unchanged on the next request.

## Development

```sh
make build   # bin/grimalkin
make test    # import check, go vet, go test ./...
make clean
```

Tests live beside the code. The core is tested against port mocks; the
Anthropic adapter against a local `httptest` server streaming SSE.
