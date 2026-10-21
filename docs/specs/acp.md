# ACP agent

The client half of this design is hearth's `docs/specs/acp-backend.md`.

## Problem Statement

grimalkin can only be used through its own TUI. It has no headless mode, so
nothing else can drive it: not hearth, which runs coding agents as backends
behind its own sessions, permissions and tools; not an editor; not a script.

## Solution

grimalkin becomes an Agent Client Protocol (ACP) agent. `grimalkin acp`
speaks ACP v1 over stdio, so any ACP client can drive it, and hearth gains a
generic ACP backend with grimalkin as its first agent.

Three things hearth needs are outside stable ACP. Each is negotiated through
a capability, so a client that doesn't use them still works:

- **Mid-turn steering** through `_session/steering`, the de facto convention
  claude-agent-acp and codex-acp ship.
- **The client's tools** over MCP-over-ACP (a draft RFD): the client's MCP
  server answers on the ACP pipe itself, with no subprocess.
- **Appended system-prompt text** in a neutral `_meta` key on `session/new`.

The TUI is unchanged. The protocol lives in a new driving adapter beside it.

## User Stories

1. As an ACP client, I want to start a grimalkin session over stdio, so that I can drive grimalkin without its TUI.
2. As an ACP client, I want reply text streamed as it's written, so that my user sees the reply form.
3. As an ACP client, I want every tool call (shell, search, web, skills and my own tools) to ask me for permission before it runs, so that I decide what grimalkin may do.
4. As an ACP client, I want a denied tool call to go back to the model as an error, so that the turn carries on without it.
5. As an ACP client, I want to offer grimalkin my own tools over the ACP connection, so that the model can call them without me running a separate MCP server.
6. As hearth, I want grimalkin never to offer its `agent` tool, so that every subagent is started through my `start_agent`.
7. As an ACP client, I want a message sent during a turn to reach the model at the next tool-round boundary, so that my user can steer without cancelling.
8. As an ACP client, I want an `injected` reply to mean the message will reach the model, so that I can mark it delivered then.
9. As an ACP client, I want a steer sent while grimalkin is idle to be refused with `promptRequired`, so that a steer never starts a turn I didn't ask for.
10. As an ACP client, I want a steer still held when the turn is cancelled to stay in the conversation, so that a message shown as delivered is never lost.
11. As an ACP client, I want `session/cancel` to stop the turn and the prompt to answer `cancelled`, so that my user can stop runaway work.
12. As an ACP client, I want a turn that fails (a bad key, a provider error) to answer with an error, so that a session never hangs.
13. As an ACP client, I want to reopen a session by id and have its history replayed, so that my user sees it and the next prompt continues it.
14. As an ACP client, I want the model and effort exposed as config options listing every model in grimalkin's config, so that my user can choose and switch them.
15. As a grimalkin user, I want a provider's key command to run only when one of its models is first picked, so that I'm not prompted for providers I don't use.
16. As an ACP client, I want my system-prompt text appended to grimalkin's, so that my instructions apply.
17. As a grimalkin user, I want `grimalkin acp` to read the same config as the TUI, so that providers, models and keys are set up once.
18. As a grimalkin user, I want sessions saved to disk, so that a session outlives its process.
19. As a grimalkin user, I want the TUI unchanged, so that headless mode costs interactive use nothing.
20. As a grimalkin developer, I want the protocol in an adapter that drives the core like the TUI does, so that there are two drivers of one core.

## Implementation Decisions

- **Protocol.** ACP v1 over stdio, JSON-RPC, on the agent side of
  `github.com/coder/acp-go-sdk`.
  - Rejected: our own stream-json-like protocol. It would need the same tool,
    permission, resume and config designs ACP already settles, and only
    hearth and grimalkin would ever speak it.
  - Rejected: wire compatibility with Claude Code's stream-json, which copies
    the parts we want to avoid.
- **A subcommand and a driving adapter.** `grimalkin acp` starts the adapter
  instead of the TUI, from the same config. The adapter drives the core as the
  TUI does and implements `core.Transcript` to turn core events into
  `session/update` notifications.
- **One session per process,** matching how hearth starts one backend process
  per session.
- **`initialize`** advertises `loadSession: true`, `mcpCapabilities.acp: true`
  and `_meta.steering.supported: true`.
- **Turns.**
  - `session/prompt` runs a parent turn.
  - Reply text streams as `agent_message_chunk`, and tool calls as
    `tool_call` and `tool_call_update`.
  - The core has no "turn ended" event, so the adapter answers the prompt
    when the turn returns:
    - `end_turn`
    - `cancelled` after `session/cancel`
    - an error after `TurnDiscarded`
- **Cancel.** `session/cancel` calls the core's `Cancel`.
- **Tools.**
  - `shell`, `search`, web search and fetch, and skills, as the config enables
    them, plus the client's tools.
  - The `agent` tool is never offered: ACP has no stable way to show a child's
    events, and hearth starts subagents itself. No flag is involved.
- **Permission gate.**
  - Every tool call goes through `session/request_permission` before it runs,
    carrying the tool's name and input. That includes the client's own tools.
  - A denial goes to the model as an error result.
  - The gate is the core's `Gate` port, which the core asks between
    `ToolStarted` and running the tool, with the call's id, name and input.
    The adapter implements it.
  - grimalkin applies no permission modes and offers no `mode` option; the
    client owns that policy.
  - Rejected: skipping the gate for the client's own tools. The "always fine"
    rule belongs to the client, beside its modes.
- **Client tools.**
  - An MCP client that speaks MCP over the ACP connection, using the draft
    acp-go-sdk v0.13.5 implements. `mcp/connect` names the server by the `id`
    from `mcpServers` and returns a `connectionId`. `mcp/message` carries
    `tools/list` and `tools/call` on that connection.
    - The current RFD is stateless, keyed by `serverId` and `requestId`. We
      move to it when the SDK does; only the framing changes.
    - Rejected: hand-rolling the current RFD's framing around the SDK.
    - Rejected: a private `_` method.
  - Each listed tool becomes a `core.Tool`. Name clashes with grimalkin's tools
    or skills fail as the core already does.
  - Only `type: "acp"` servers are supported.
- **Steering.**
  - The wire shape matches claude-agent-acp and codex-acp: the request is
    `{sessionId, prompt: ContentBlock[]}`, and the capability is
    `_meta.steering.supported` at the top level of the `initialize` response.
  - During a turn, the message is held and the reply is
    `{outcome: "injected"}`.
  - **`injected` means delivered.** Between tool rounds the core takes the held
    messages into the conversation as user content. If the turn ends or is
    cancelled before the next round, the held messages are still appended to
    the history, so the next prompt sees them. No message is ever echoed.
  - With no turn running, the reply is
    `{outcome: "promptRequired", reason: "noRunningTurn"}`, and nothing is
    started. That is the opt-in behaviour of claude-agent-acp
    (`_meta.steering.idleBehavior: "promptRequired"`), and grimalkin always
    behaves this way.
  - The core gains a seam: a source of pending user messages it drains between
    tool rounds and at the end of the turn.
  - Rejected: echoing each message with the client's uuid when it lands. No
    other agent does it, so it would be grimalkin's private convention.
- **Appended system prompt.** `_meta.systemPrompt.append` on `session/new` and
  `session/load` is appended to the system prompt. claude-agent-acp accepts the
  same key and shape, so a generic client can send it to any agent.
- **Config options.**
  - `session/new` returns `configOptions`:
    - `model`: every model in the config, written `provider/model`
    - `thought_level`: the current model's efforts
  - `session/set_config_option` switches either one, and the response lists
    the new model's efforts.
  - The default is the config's top-level selection.
  - A provider's key command runs the first time one of its models is picked,
    not at startup.
  - Rejected: offering only the selection and `subagents.models`. That list
    limits the `agent` tool, which is off here.
  - Key commands run with no terminal. When one fails, `set_config_option`
    returns an error carrying the command's message, and the session stays on
    its current model.
    - Rejected: running every provider's key at startup, which prompts for
      providers that are never picked.
  - When the next request goes to a different model from the one that wrote
    earlier turns' thinking blocks, those blocks are dropped. A model can't
    verify another model's signatures, so nothing usable is lost.
    - Rejected: sending them as they are and letting the provider fail the
      turn.
    - Rejected: allowing switches only within one provider.
- **Persistence and resume** (wishlist #3).
  - Each session is one JSONL file named by its ACP `sessionId`, in
    `~/.local/state/grimalkin/sessions/`.
  - Messages are stored in the provider's own form, because `Message.Content`
    is opaque to the core.
  - `session/load` restores the conversation, replays it as `session/update`
    notifications, then answers.
  - The core gains a way to seed its history.
- **Order.** Each slice is demoed with hearth running the real
  `grimalkin acp`:
  1. One text turn with no tools.
  2. Persistence and `session/load`. hearth drops processes on daemon
     shutdown, crash and delete, and resumes by session id, so this comes
     before anything else is built on top.
  3. Tools and the gate, then client tools over MCP-over-ACP.
  4. Steering.
  5. Models and efforts.

## Testing Decisions

- **One seam: the ACP connection.**
  - Tests drive the adapter with acp-go-sdk's client side over in-memory pipes,
    with a scripted fake `core.Model`. They check messages in and messages
    out.
  - The MCP client, the session store and the steering queue are not tested
    alone.
- **Cases:**
  - A turn streams chunks and ends `end_turn`; cancel ends `cancelled`.
  - A tool call asks permission, and a denial reaches the model as an error.
  - A client tool is listed and called through `mcp/message`.
  - The `agent` tool is absent.
  - A steer during a turn reaches the model at the next tool-round boundary.
  - A steer while idle gets `promptRequired`.
  - A steer held when the turn is cancelled, or racing the end of the turn, is
    in the history the next prompt sends.
  - `session/load` replays a saved session, and the next prompt continues it.
  - `set_config_option` switches the model and lists its efforts.
  - A provider's key command runs only when its model is first picked.
  - The appended text reaches the model's system prompt.
- **Prior art:** the mock model, tool and transcript in the core's agent tests.
- **End to end:** each slice's demo, through hearth.

## Open Questions

- Should tool output stream in `tool_call_update`? Slice 3 sends it only at the
  end, as the core's `ToolFinished` carries it.
- What does a `model` change mid-turn do: apply from the next model call, or be
  refused until the turn ends?
## Out of Scope

- The `agent` tool and children under ACP, including the draft subagents RFD.
- Stdio, HTTP and SSE MCP servers.
- Permission modes in grimalkin, and a sandbox for its shell.
  - Later: grimalkin's own `auto` mode, as Claude Code has. grimalkin would
    advertise `auto` in a `mode` config option and classify each call, sending
    `request_permission` only for calls it won't approve itself. hearth would
    then hand `auto` to the agent instead of always asking, as it does now for
    ACP agents. It depends on the sandbox: the wishlist makes a sandbox the
    prerequisite for any auto-approve.
- A flag to turn off built-in tools in favour of the client's.
- Unqueue, rewind and fork.

## Further Notes

- **Security.** `grimalkin acp` hands shell access to whatever client drives
  it, gated only by that client's permission answers. This crosses a trust
  boundary and should get a human reviewer.
- Wishlist #6's headless `grimalkin -p` sketch is replaced by this.
- claude-agent-acp has an open issue (#1027) where a steered turn can hang
  without answering `session/prompt`. The steer-racing-turn-end case guards
  against the same bug here.
