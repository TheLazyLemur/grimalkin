# grimalkin

A terminal coding agent with vim-style navigation, written in Go.

grimalkin speaks the Anthropic Messages API through the official Go SDK, so it
works with Claude or any compatible endpoint (e.g. Ollama). It renders with
Bubble Tea and Lip Gloss and has one tool: a shell.

> **Warning:** the shell is **unsandboxed**. Every command the model chooses
> runs as `/bin/sh -c` in the current directory, with your permissions and no
> timeout.

**Contents:** [Quick start](#quick-start) ·
[Configuration](#configuration) · [Keys](#keys) ·
[Architecture](#architecture) · [Development](#development)

## Quick start

Requires Go 1.26 on macOS or Linux.

```sh
export ANTHROPIC_API_KEY=...
make run
```

`make run` builds `bin/grimalkin` and starts it.

## Configuration

Configuration is by environment variable only.

| Variable             | Default         | Purpose                        |
| -------------------- | --------------- | ------------------------------ |
| `AGENT_MODEL`        | `claude-opus-5` | Model name                     |
| `ANTHROPIC_API_KEY`  | —               | API key (read by the SDK)      |
| `ANTHROPIC_BASE_URL` | Anthropic API   | Endpoint (read by the SDK)     |

Any other variable the Anthropic SDK reads (e.g. `ANTHROPIC_AUTH_TOKEN`) works
too. To use Ollama's Anthropic-compatible endpoint:

```sh
ANTHROPIC_API_KEY=ollama ANTHROPIC_BASE_URL=http://localhost:11434 \
  AGENT_MODEL=deepseek-v4.1-flash:cloud ./bin/grimalkin
```

## Keys

grimalkin has three vim-style modes, shown in the status bar. It starts in
INSERT. Words are whitespace-separated (vim's `W`/`B`/`E`).

### Everywhere

| Key                   | Action                                               |
| --------------------- | ---------------------------------------------------- |
| `Ctrl-C`              | Stop the running turn (dropped from history); quit when idle |
| `PgUp` / `Ctrl-b`     | From the input: jump into VIEW and page up           |

### INSERT — type a message

| Key                                   | Action                          |
| ------------------------------------- | ------------------------------- |
| `Enter`                               | Send                            |
| `Shift+Enter`, `Alt+Enter`, `Ctrl-J`  | Newline                         |
| `Ctrl-W` / `Ctrl-U`                   | Delete word back / to line start |
| `↑` `↓`                               | Move between lines              |
| `Esc`                                 | → NORMAL                        |

Bracketed paste goes in as-is, newlines included.

### NORMAL — edit the input

| Key                                  | Action                     |
| ------------------------------------ | -------------------------- |
| `h j k l w b e 0 ^ $`                | Move                       |
| `x X r D C`                          | Edit                       |
| `d` `c` `y` + motion, `dd cc yy`     | Operators (doubled = line) |
| `o O` · `p P` · `u`                  | Open line · paste · undo   |
| `Enter`                              | Send                       |
| `i a I A`                            | → INSERT                   |
| `k` on first line, `Ctrl-w k`        | → VIEW                     |

### VIEW — navigate the transcript

| Key                                  | Action                                     |
| ------------------------------------ | ------------------------------------------ |
| `h j k l w b e 0 $ gg G`             | Move                                       |
| `Ctrl-d/u/f/b`, `PgUp` `PgDn`        | Scroll                                     |
| `{` `}`                              | Previous / next message or command block   |
| `/` `?`, then `n` `N`                | Search (smartcase, highlighted); `Esc` clears |
| `v` `V`, `o`                         | Visual select; swap ends                   |
| `y`, `yy`, `y$`                      | Yank                                       |
| `j` past last line, `Ctrl-w j`       | → NORMAL                                   |
| `i a o`                              | → INSERT                                   |
| `q`                                  | Quit                                       |

Yanks go to a register shared with NORMAL's `p`, and to the macOS clipboard.
Wrapped lines are rejoined; bullets and padding are dropped.

### Terminal support

`Shift+Enter` needs the kitty keyboard protocol (kitty, Alacritty, Ghostty,
WezTerm, foot, recent iTerm2); grimalkin enables it on start and disables it on
exit. Elsewhere, use `Alt+Enter` or `Ctrl-J`. Output uses 256 colours.

Replies render as markdown (headings, lists, quotes, rules, fenced code, inline
bold/italic/code/links). Shell commands and their output sit in a shaded block.
Commands get a closed stdin; `Ctrl-C` kills a running command and its whole
process group.

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
