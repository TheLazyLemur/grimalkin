# Contributing

## Architecture

Ports and adapters: a pure domain core defines the ports it needs, adapters
implement or drive them, and one composition root wires them together.

```
main.go                   configuration and composition root
internal/core/            conversation, turn loop, tool rules, events,
                          ports (Model, Tool, Transcript)
internal/adapters/
  anthropic/              outbound: Model port over anthropic-sdk-go
  tools/                  outbound: tools (tools.Shell)
  clipboard/              outbound: tui.Clipboard via pbcopy
  tui/                    inbound: Bubble Tea UI, editor, navigation,
                          markdown; drives the core via tui.Agent
scripts/check-imports.sh  enforces the import rules
```

**Import rules** (checked by `make check`):

- The core imports nothing from the application.
- An adapter imports only the core, never another adapter.
- Only `main.go` sees concrete adapters.

**Ports.** The core's outbound ports live in `internal/core/ports.go`. The TUI
defines the ports it consumes (`tui.Agent`, satisfied by `*core.Agent`, and
`tui.Clipboard`), since Go interfaces belong with their users.

**Events.** The core reports a turn to `Transcript` as events
(`internal/core/events.go`): reply started, reply text, tool started, tool
finished, turn discarded. The TUI decides how each looks.

**Tools.** A tool implements `core.Tool`: a spec, a `Summary` of a call as a
command line, and `Run`, which returns output or an error and must honour its
context. Tools never touch the screen. The core looks tools up by name, answers
unknown names with an error, and caps each result at 30 KB (keeping the tail).

To add a tool, add a type to `internal/adapters/tools` and list it in
`toolCatalogue` in `main.go`.

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
