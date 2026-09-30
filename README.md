# grimalkin

A terminal coding agent with vim-style navigation, written in Go.

grimalkin speaks the Anthropic Messages API through the official Go SDK, so it
works with Claude or any compatible endpoint (e.g. Ollama). It renders with
Bubble Tea and Lip Gloss and has one tool: a shell.

> **Warning:** the shell is **unsandboxed**. Every command the model chooses
> runs as `/bin/sh -c` in the current directory, with your permissions and no
> timeout.

**Contents:** [Quick start](#quick-start) ·
[Configuration](#configuration) · [Keys](#keys)

Working on grimalkin itself? See [CONTRIBUTING.md](CONTRIBUTING.md).

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
