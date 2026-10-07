# grimalkin

A terminal coding agent with vim-style navigation, written in Go.

grimalkin speaks the Anthropic Messages API through the official Go SDK, so it
works with Claude or any compatible endpoint (e.g. Ollama). It renders with
Bubble Tea and Lip Gloss and has two tools, a shell and
[search](#search), plus [skills](#skills).

> **Warning:** the shell is **unsandboxed**. Every command the model chooses
> runs as `/bin/sh -c` in the current directory, with your permissions and no
> timeout.

**Contents:** [Quick start](#quick-start) ·
[Configuration](#configuration) · [Search](#search) · [Skills](#skills) ·
[Keys](#keys)

Working on grimalkin itself? See [CONTRIBUTING.md](CONTRIBUTING.md).

## Quick start

Requires Go 1.26 on macOS or Linux.

Write a [config file](#configuration), then:

```sh
make run
```

`make run` builds `bin/grimalkin` and starts it.

## Configuration

grimalkin reads `~/.config/grimalkin/config.json` (on every platform) and
nothing else. It refuses to start if the file is missing, has a key it doesn't
recognise, or selects something it doesn't list.

```json
{
  "provider": "ollama",
  "model": "glm-5.3-flash:cloud",
  "effort": "high",

  "providers": {
    "anthropic": {
      "wire": "anthropic-messages",
      "baseUrl": "https://api.anthropic.com",
      "apiKey": "!printenv ANTHROPIC_API_KEY",
      "thinking": "adaptive",
      "efforts": ["low", "medium", "high", "xhigh", "max"],
      "models": {
        "claude-opus-5": {}
      }
    },
    "ollama": {
      "wire": "anthropic-messages",
      "baseUrl": "http://localhost:11434",
      "apiKey": "ollama",
      "thinking": "enabled",
      "models": {
        "glm-5.3-flash:cloud": { "efforts": ["low", "high", "max"] },
        "gemma4:31b-cloud": {}
      }
    }
  }
}
```

`provider`, `model` and `effort` choose what to run; all three are required.
Each provider is an endpoint:

| Field      | Meaning                                                         |
| ---------- | --------------------------------------------------------------- |
| `wire`     | Request format. Only `anthropic-messages` exists.               |
| `baseUrl`  | Endpoint.                                                       |
| `apiKey`   | The key, or a command to print it if it starts with `!`.        |
| `thinking` | How the endpoint turns thinking on: `adaptive`, `enabled` or `disabled`. |
| `efforts`  | Effort names the models accept, sent as-is. Optional.           |
| `models`   | The models it offers.                                           |

A model can override `thinking` and `efforts`. A model with no `efforts`, on
itself or its provider, has no effort levels: grimalkin sends none and the
status bar shows `effort n/a`.

A key command runs once at startup, for the selected provider only, and can
prompt in the terminal. grimalkin ignores `ANTHROPIC_*` environment variables.

What to put where:

- **Claude** accepts `adaptive` thinking; 5.x models reject `enabled`.
- **Ollama** only turns thinking on for some models with `enabled`. To find a
  model's effort levels, run
  `curl localhost:11434/api/show -d '{"model":"<name>","verbose":true}'` and
  look at `thinking.values`; `false` and `true` are on/off, not efforts.

## Search

The `search` tool finds code by meaning. It shortlists files by keyword
locally, sends excerpts of the shortlist to TypeSafe's
[Jev](https://docs.typesafe.ai) to score their relevance to the question, and
returns the best files with line ranges. The model reads those with the
shell; its tool description tells it to fall back to `rg` and the shell when
search doesn't find what it needs.

It is on in every project once the config has a `search` block:

```json
"search": {
  "apiKey": "!sed -n 's/^JEV_API_KEY=//p' ~/.jev",
  "model": "jev-latest"
}
```

`apiKey` is required and works like a provider's (a leading `!` runs a
command once at startup). `model` defaults to `jev-latest`. Without the block
there is no search tool.

> **Warning:** search sends file contents to TypeSafe, in every project. Only
> files the keywords match are sent, at most 60 files and 384 KB per search.
> It needs ripgrep, honours `.gitignore`, reads only source and text file
> types, and never sends hidden files and directories, dependency and build
> directories (`node_modules`, `vendor`, `bin`, `dist`, …), files over
> 256 KB, binary files, or files that look like they hold a key or password.

## Skills

grimalkin loads [Agent Skills](https://agentskills.io): directories holding a
`SKILL.md`. It looks for them only in the directories you list under
`skillDirs` in the config; with none listed, there are no skills.

```json
"skillDirs": ["~/.config/grimalkin/skills", ".agents/skills"]
```

Each entry is a directory of skill directories (`<entry>/<name>/SKILL.md`). A
leading `~` is your home directory; a relative entry is relative to the
working directory, so `.agents/skills` means the current project's. Entries
that don't exist are skipped. When two skills share a name, the one in the
earlier entry wins and the startup info lines say which was shadowed, along
with any skill that couldn't be loaded.

The model sees each skill's name and description in its system prompt and
loads one with the `skill` tool. You load one by sending `/name arguments`.
Loading renders the skill the way Claude Code does:

- **Arguments:** `$ARGUMENTS` is everything after the name, as typed;
  `$ARGUMENTS[N]` and `$N` pick one (0-based, shell-style quoting); `$name`
  picks one by the names in the `arguments` field. If nothing uses them,
  they are appended as `ARGUMENTS: …`. `\$1` is a literal `$1`.
- **Variables:** `${GRIMALKIN_SKILL_DIR}` and `${GRIMALKIN_PROJECT_DIR}`.
- **Commands:** `` !`cmd` `` (at a line start or after whitespace) and
  ` ```! ` fenced blocks run with `/bin/sh` in the working directory before
  the model sees the skill, and their output (stdout and stderr) replaces
  them. A non-zero exit aborts the load, except exit 1 from `grep`, `rg`,
  `find`, `diff`, `test`, `git diff` and similar. Each command has 2 minutes.

> **Warning:** like the shell tool, these commands are **unsandboxed**, and
> they run without showing up as tool calls. Only list skill directories you
> trust.

A skill is named by its directory, as in Claude Code; a frontmatter `name`
that differs only earns a warning. Frontmatter fields acted on:
`description` (defaults to the body's first line), `when_to_use`,
`arguments`, `disable-model-invocation` (only you can load it) and
`user-invocable: false` (only the model can). Others, such as
`allowed-tools`, `model` or `context`, are ignored.

The skill reaches the model without its frontmatter, wrapped in a
`<skill_content>` block that names its directory and lists its bundled files.
Loading a skill whose rendered content is already in the conversation sends a
short note instead of a second copy.

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
| `za`                                 | Expand / collapse output under the cursor  |
| `zR` `zM`                            | Expand / collapse all tool output          |
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
bold/italic/code/links) in a muted dark palette with a moss accent. Code sits
behind a thin gutter instead of a shaded block, and margins shrink on narrow
terminals. Tool output starts collapsed: dimmed, showing its last 4 lines under
a count of the rest. Skill content shows only its line count. Expanded output
sits behind the gutter. Errors are never collapsed. Commands get a closed
stdin; `Ctrl-C` kills a running command and its whole process group.
