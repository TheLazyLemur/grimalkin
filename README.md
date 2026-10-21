# grimalkin

A terminal coding agent with vim-style navigation, written in Go.

grimalkin speaks the Anthropic Messages API through the official Go SDK, so it
works with Claude or any compatible endpoint (e.g. Ollama). It renders with
Ultraviolet and has a shell, [search](#search), [web](#web) tools,
[skills](#skills) and [subagents](#subagents).

The client can offer its own tools too: a `session/new` or `session/load`
`mcpServers` entry of `type: "acp"` is an MCP server the client answers over
the ACP connection itself (the draft in acp-go-sdk v0.13.5: `mcp/connect`,
then `mcp/message`). Its tools are listed before the session starts and
offered to the model beside grimalkin's own, each behind the same permission
request. A tool named like one of grimalkin's, or like another of the
client's, fails the session start. Servers of other types (stdio, http, sse)
are not supported and are ignored, with a note on stderr.

> **Warning:** the shell is **unsandboxed**. Every command the model chooses
> runs as `/bin/sh -c` in the current directory, with your permissions and no
> timeout.

**Contents:** [Quick start](#quick-start) ·
[Configuration](#configuration) · [Search](#search) · [Web](#web) ·
[Skills](#skills) · [Subagents](#subagents) · [ACP](#acp) · [Keys](#keys)

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

A key command runs once at startup, only for the selected provider and any
provider a [`subagents`](#subagents) entry names, and can prompt in the
terminal. Under [`grimalkin acp`](#acp) only the selected provider's runs at
startup; another's runs the first time one of its models is picked, once per
process. grimalkin ignores `ANTHROPIC_*` environment variables.

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

`apiKey` works like a provider's (a leading `!` runs a command once at
startup). `model` defaults to `jev-latest`. Without the block there is no
search tool. Nor is there one when the block has no key, or its key command
fails or prints nothing: grimalkin still starts, and an info line says why.

`"map": true` turns on the repo map, a prototype. It is built in the
background: Jev tags every code and config file
search may send (not tests or docs, up to 3,000) with its role and the
concepts it is about, drawn from the repo's own folder and identifier names.
Later searches map the query onto those concepts and add matching files to
the shortlist, even when they share no word with the query, and label results
with their role. A search before the map is ready uses keywords only.

In a git repository the map starts at the first search, or at startup with
`"mapAtStartup": true` so it is likely ready by then. Anywhere else it is
built only when you type `/index`, which also builds it again after a build
that failed. On a repo of about 2,000 files the first build takes under two
minutes and costs about $0.17: it sends the first 8 KB of every one of those
files to TypeSafe. At startup, that happens whether or not you search.

The map is stored in the project, in `.grimalkin/map.json`, and the
directory ignores itself in git. Later sessions read it and send only files
that are new or whose first 8 KB changed, so they start with the map nearly
ready. The concepts stay as the first build chose them; `/index` on a map
that is already built builds it from scratch, with fresh concepts. A stored
map made for another Jev model, or holding anything a build here could not
have written, is ignored and built again.

> **Warning:** search sends file contents to TypeSafe, in every project. A
> search sends excerpts of at most 60 files: up to 384 KB in a first pass,
> then up to 40 more excerpts of those files (320 KB) in a second. Without
> the repo map, only files the keywords match are sent. With it, a search
> also sends files the map matched, and building the map sends the first
> 8 KB of up to 3,000 files, whatever you search for (only the new and
> changed ones once a map is stored).
> Search needs ripgrep, honours `.gitignore` in a git repository, reads only
> source, config and text file types, and never sends hidden files and
> directories, dependency and build directories (`node_modules`, `vendor`,
> `bin`, `dist`, …), files over 256 KB, binary files, or files that look
> like they hold a key or password.


## Web

`web_search` and `web_fetch` let the model search the web and read a page.
They are on once the config has a `web` block naming a provider:

```json
"web": {
  "ollama": { "apiKey": "!cat ~/.ollama_key" }
}
```

Only `ollama` exists so far: [Ollama's web search and fetch](https://docs.ollama.com/capabilities/web-search),
which needs a key from an ollama.com account, not the placeholder a local
Ollama server takes. `apiKey` works like a provider's.

One provider serves both tools. If the block names several, the first by
name is used, and only its key command runs. A line at startup always says
which provider is in use, or that web is off: with no block, an empty one,
or a key command that fails or prints nothing. A name grimalkin doesn't know
stops it from starting.

Every query the model searches for and every URL it fetches goes to the
provider. What comes back is text from the web, read by a model that can run
shell commands: the tools tell it to treat that text as data, which lowers
the risk of a page steering it but does not remove it.

`web_search` returns up to 10 results (5 unless the model asks), sharing
20 KB of content between them. `web_fetch` returns the first 20 KB of a
page and up to 4 KB of its links.

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

## Subagents

The `agent` tool lets the model hand a self-contained task to a subagent: a
child agent that starts with only that task, works through it with its own
tools and context, and returns only its final reply. The reading behind the
answer stays out of your conversation. The tool is always on and needs no
config.

A subagent runs on the session's provider and model unless the model picks
another you have allowed (below). It gets the
shell and your skills, but not search or the web tools, and it cannot start
subagents of its own. The reply comes back to the model as `[agent N]`
followed by the text; passing that number back as `agent` sends the same
subagent a follow-up, with its history intact. Subagents are numbered from 1
and last until you quit.

An optional `subagents` block in the config lists the other models subagents
may run on:

```json
"subagents": {
  "models": [
    { "provider": "ollama", "model": "glm-5.3-flash:cloud", "effort": "low" },
    { "provider": "ollama", "model": "gemma4:31b-cloud" }
  ]
}
```

Each entry names a declared provider and one of its models, once, and not
the selected model, which subagents use anyway. `effort` is optional: it must
be one of that model's efforts. An entry without one takes the top-level
`effort`; if its model doesn't list that, the next higher level it does list,
going up the selected model's `efforts`, or failing that its own highest. So
write every `efforts` list lowest first. A model with no effort levels gets
none, and so does every such entry when the selected model has none.
grimalkin refuses to start on an entry that doesn't
check out, and runs the key command of every provider an entry names at
startup. A startup line lists the allowed models. The `agent` tool then
takes a `model`, one of `provider/model` for the session's model and each
entry; a follow-up to a subagent stays on the model it started on. A
subagent on another provider sends its task, and whatever it reads with the
shell, to that provider.

Several `agent` calls in one response run at the same time, as do a
response's other tool calls. At most five subagents run at once; further
ones wait for a slot. The parent's turn waits for all of them.

Each subagent shows as one line in the transcript, updated in place:
`agent 1 · ollama/glm-5.3-flash:cloud · 3 tools · running`, or `waiting`
while it has no slot. Its work and its reply are not shown there: each
subagent has a buffer of its own, a transcript like the session's. In VIEW,
`gf` on a subagent's line opens its buffer, `:ls` lists the buffers and
`:b N` shows buffer N, the session's own being buffer 0.

The input, `Enter`, `Ctrl-C` and the status bar's model, effort and
`working` or `ready` go by the buffer on screen. In a subagent's buffer, `Enter` sends your message to
that subagent as a follow-up, once it has finished its turn, and the answer
appears there: the session's model is not told. A message is sent as typed,
so `/name` loads no skill there. `Ctrl-C` stops only that subagent, and the
call that started it is answered "cancelled by user". A turn you start on a
subagent takes one of the five places and adds a line for it to the
session's transcript; `Ctrl-C` in the session's buffer does not stop it.

In the session's buffer, `Ctrl-C` stops the turn and every subagent it started.

> **Warning:** a subagent's shell is the same **unsandboxed** shell, and its
> commands do not appear in the session's transcript, only the count of them:
> open its buffer to see them.

## ACP

`grimalkin acp` runs grimalkin as an [Agent Client Protocol](https://agentclientprotocol.com)
agent on stdin and stdout instead of the TUI, from the same config. It is
work in progress: one session per process, and one turn at a time.
Reply text streams as `agent_message_chunk` updates. `session/cancel` stops a
turn, and a turn that fails answers `session/prompt` with an error.
`_session/steering` (as claude-agent-acp and codex-acp have it, advertised in
`initialize`'s `_meta.steering`) sends a message during a turn: it answers
`injected`, and the model reads it after the current tool round, or, if the
turn ends or is cancelled first, with your next prompt. With no turn running
it answers `promptRequired` and starts nothing.

The model gets the same tools as in the TUI (the shell, and search, web and
skills when configured) but never the `agent` tool. Each call is announced
as a `tool_call` update (`kind`, `rawInput`, and the tool's name in
`_meta.toolName`) and then waits on a `session/request_permission` to the
client, with `allow_once` and `reject_once` options. Only the client's
allow runs it; a denial, an error or a cancel does not, and the model gets
a denial as an error result. The call then goes `in_progress` and ends in a
`tool_call_update` that is `completed` or `failed`, with the output.

> **Warning:** the shell is **unsandboxed**, and the client's answers are
> all that stand between the model and it. A client that approves every
> request gives the model your shell.

Each session is saved as it goes, to
`$XDG_STATE_HOME/grimalkin/sessions/<sessionId>.jsonl` (by default
`~/.local/state/grimalkin/sessions/`), readable by you alone since it holds
the whole conversation. `session/load` with that id, in a new process,
replays the session's finished turns (prompts, replies, tool calls and steers) and
continues it; a session file whose last line is torn, from a process that
died mid-write, loads without that line; a cancelled or failed turn is neither replayed nor remembered, except for its steers. If a
write fails, the error goes to stderr and every later prompt is refused, as
the session could no longer be resumed.

`session/new` and `session/load` answer with two config options. `model`
lists every model in the config as `provider/model`, each with its efforts,
lowest first, in `_meta.efforts` (`[]` when it has none); `thought_level`
lists the current model's efforts, and is left out when it has none. A
session starts on the config's `provider`, `model` and `effort`.
`session/set_config_option` switches either from the next turn on (a running
turn finishes on its model) and answers with the options as they now are.
A new model keeps the effort if it lists it, else takes the next higher one
it lists, going up the old model's `efforts`, else its own highest. The
first pick of a model on another provider runs that provider's key command;
if it fails, the answer is an error carrying the command's stderr and the
session stays where it was. The choice is saved with the session and
restored by `session/load`; if the saved model is no longer in the config
or its key command fails, the load goes on with the config's model and says
why on stderr.

`session/close` (advertised as `sessionCapabilities.close`) cancels a running
turn and ends the session; a session that never ran a turn has its file
deleted. The other capabilities are not advertised yet. stdout carries
protocol messages only; errors go to stderr. Key commands run with no stdin,
so one that prompts fails, and with their stderr captured for the error.

## Keys

grimalkin has three vim-style modes, shown in the status bar. It starts in
INSERT. Words are whitespace-separated (vim's `W`/`B`/`E`).

### Everywhere

| Key                   | Action                                               |
| --------------------- | ---------------------------------------------------- |
| `Ctrl-C`              | Stop the shown buffer's running turn (dropped from history); quit when nothing is running |
| `PgUp` / `Ctrl-b`     | From the input: jump into VIEW and page up           |

### INSERT — type a message

| Key                                   | Action                          |
| ------------------------------------- | ------------------------------- |
| `Enter`                               | Send to the shown buffer's agent, unless it is working |
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
| `gf`                                 | On a subagent's line: show its buffer      |
| `:`                                  | Command line: `Enter` runs, `Esc` leaves   |
| `j` past last line, `Ctrl-w j`       | → NORMAL                                   |

Commands:

| Command  | Action                                                             |
| -------- | ------------------------------------------------------------------ |
| `:ls`    | List the buffers: the session as 0, then each subagent with its model and state; `%` marks the one shown. Any key puts the list away |
| `:b N`   | Show buffer N; `:b 0` is the session's                             |
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
terminals. Tool output starts collapsed: dimmed, showing its last 4 screen rows
under a count of the rest. Skills, whether you or the model loaded them, show
only their line count. Expanded output sits behind the gutter. Errors are never
collapsed. Commands get a closed
stdin; `Ctrl-C` kills a running command and its whole process group.
