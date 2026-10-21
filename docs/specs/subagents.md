# Subagents

## Problem Statement

grimalkin is one agent with one conversation. Heavy reading and searching
fill that conversation with output the user only needs the conclusion of, and
independent pieces of work run one after another. A model can work around
this by shelling out to another agent's CLI (GPT once fanned a review out to
eight `codex exec` processes), but that needs another tool installed and
signed in, runs on another provider's bill, shows nothing in the transcript
and survives Ctrl-C.

Agents that do have subagents treat them as black boxes: a task goes in, one
answer comes out, and the child's work is gone.

## Solution

A built-in `agent` tool. The model hands a task to a child agent, which works
through it with its own tools and context; only the child's final reply
comes back. Several children run at once.

On top of that, two things:

- **Subagents as buffers.** Each child has its own full-screen buffer for the
  session. The user can watch it work, read everything it did, and talk to it
  once it has finished. The parent can also come back to a child it started.
- **Mixed fleet.** A child can run on any model the user has allowed for
  subagents, on any configured provider, e.g. a cheap Ollama model searching
  for an Opus parent.

Optionally, a child can be a **fork**: an exact copy of the parent,
conversation and all, which reads the parent's cached prompt cheaply.

## User Stories

1. As a user, I want the model to hand a self-contained task to a subagent, so that my conversation keeps the conclusion and not the reading behind it.
2. As a user, I want several subagents to run at the same time, so that independent work finishes sooner.
3. As a user, I want tool calls in one response to run at the same time, so that several shell commands don't wait on each other.
4. As a user, I want skill loads in a response to still be deduplicated, so that the same skill isn't sent twice when calls run at once.
5. As a user, I want at most five subagents running at once, with the rest waiting their turn, so that a large fan-out doesn't swamp my machine or my rate limits.
6. As a user, I want to list the models subagents may use, so that the model can only pick ones I chose.
7. As a user, I want the model to pick a cheaper or faster model for a subagent, so that grunt work doesn't cost Opus prices.
8. As a user, I want a subagent with no model given to use the parent's model, so that the default is predictable.
9. As a user, I want to set an effort for each subagent model, so that a scout model can run at low effort.
10. As a user, I want every listed provider's key resolved at startup, so that a key command that prompts in the terminal doesn't fight the TUI later.
11. As a user, I want a config error at startup when a subagent model or effort doesn't exist, so that mistakes surface before a turn fails.
12. As a user, I want a fresh subagent to start with only its task, so that its context holds nothing it doesn't need.
13. As a user, I want a fresh subagent to know its reply goes to another agent, so that it writes a complete answer rather than a chat reply.
14. As a user, I want a fresh subagent to have the skill catalogue, so that it can load skills like the parent.
15. As a developer, I want subagents to have their own tool list, so that I can choose per tool what children get as tools are added.
16. As a user, I want subagents unable to start subagents, so that fan-out can't run away.
17. As a user, I want the model to fork a subagent from the current conversation, so that a child that needs the full context gets it without a long task description.
18. As a user, I want a fork to reuse the parent's cached prompt and thinking, so that it costs little and reasons from where the parent was.
19. As a user, I want a fork with another model rejected, so that the model learns fork means the parent's model.
20. As a user, I want a subagent's reply to say which agent it came from, so that the model can resume that agent later.
21. As a user, I want the model to send a follow-up to a subagent it started earlier, so that it can ask for more without starting over.
22. As a user, I want resuming a running subagent rejected as busy, so that a child never runs two turns at once.
23. As a user, I want resuming with a model or fork rejected, so that a resumed child keeps the setup it started with.
24. As a user, I want the parent to wait for its subagents, so that their results arrive in the same turn.
25. As a user, I want a subagent's reply capped like any tool result, so that one child can't flood the parent's context.
26. As a user, I want one status line per subagent in the parent's transcript showing its number, model, tool count and state, so that I can see the fan-out at a glance.
27. As a user, I want to list subagent buffers with `:ls`, so that I can see every child this session.
28. As a user, I want to open a buffer with `:b N`, so that I can jump to a child by number.
29. As a user, I want `gf` on a status line to open that child's buffer, so that I can go from the parent straight to the child.
30. As a user, I want to watch a running subagent's transcript live, so that I can see what it's doing.
31. As a user, I want a subagent's buffer to show its full work, so that I can check how it reached its answer.
32. As a user, I want the input in a subagent's buffer to talk to that subagent, so that I can ask it follow-ups once it has finished.
33. As a user, I want Enter ignored in a running subagent's buffer, so that I can't start a second turn on it.
34. As a user, I want my follow-ups with a subagent kept in its history, so that the parent sees them if it resumes that child.
35. As a user, I want the status bar to show the model and effort of the buffer I'm in, so that I know what I'm talking to.
36. As a user, I want Ctrl-C in the parent to cancel the turn and every subagent it started, so that one key stops everything.
37. As a user, I want Ctrl-C in a subagent's buffer to stop only that subagent, so that the parent carries on without it.
38. As a user, I want the parent told when I stopped a subagent, so that it doesn't treat the silence as an answer.
39. As a user, I want a failed or cancelled subagent's buffer kept and marked, so that I can see what it did before it stopped.
40. As a user, I want a failed subagent to give the parent the reason, so that the parent can retry or work around it.
41. As a user, I want subagent buffers to last for the session, so that I can come back to them until I quit.

## Implementation Decisions

- **The `agent` tool.** Parameters: `task` (required); `model` (optional, an
  enum of the allowed subagent models as `provider/model`, defaulting to the
  parent's model); `fork` (optional, default false); `agent` (optional, the
  number of an earlier child to resume, with `task` as the follow-up). The
  core rejects `agent` with `model` or `fork`, `fork` with a model other than
  the parent's, a busy child, and an unknown number. The result is
  `[agent N]` followed by the child's final text reply, under the existing
  30 KB tool result cap.
- **Children are agents.** A child runs the same turn loop as the parent,
  with its own history, its own 50-model-call cap per turn and the same
  discard-on-failure rule. The parent's call blocks until the child's turn
  ends.
- **Fresh children** get the base system prompt, the skill catalogue and a
  short subagent note (the reply goes to the agent that started it, not the
  user, and must be complete). Their tools come from a separate subagent
  tool list built in the composition root, initially the shell plus the
  skill tool. The `agent` tool is never on it.
- **Forks** send the parent's exact system prompt and tool list and a copy
  of the parent's history up to and including the response being handled,
  with thinking intact. That response holds tool calls, the fork's own
  `agent` call among them, and each needs a result before any user text, so
  the fork's history ends with one user message: a placeholder result for
  every tool call in the response (its own call's says it is the fork that
  call started; the others' say the call is running in the parent), then
  the task as text. That keeps the prompt cache and thinking valid (both
  are bound to the system prompt, tools and model). The parent's own
  history is not changed by the fork. A fork sees the `agent` tool; the core answers its calls with an
  error ("subagents can't start subagents").
- **Concurrent tool calls.** All tool calls in a response run at once, shell
  included, except skill tool calls, which run first and in order so the
  "already loaded" dedup within a response keeps working. Results go back in
  call order. At most five children run at once across the session; further
  `agent` calls wait for a slot.
- **Status lines.** Each child has one line in the parent's transcript,
  updated in place: `agent N · <model> · <count> tools · <state>`. A child
  waiting for a slot shows `agent N · <model> · waiting`, with no tool
  count. The status line is the `agent` call's only entry in the parent's
  transcript: the call gets no tool started or finished entry, and the
  child's reply is not shown there.
- **Numbering and lifetime.** Children are numbered 1, 2, 3… per session and
  live, with their histories, until grimalkin exits.
- **Ports.** The core needs a `Model` for each allowed subagent model, keyed
  by `provider/model`, alongside the parent's. Transcript events carry the
  number of the agent they belong to (the parent being agent 0), plus events
  for a child's lifecycle: waiting (number, model), started (number, model),
  progress (tool count), finished, failed and cancelled. Lifecycle events
  belong to the parent, since its transcript shows them, and name the child
  they are about.
- **Cancelling.** Cancelling the parent's turn cancels every child it
  started. Cancelling one child ends only that child's turn; the parent's
  call returns "cancelled by user" as an error result and the parent goes
  on. A cancelled or failed child's history keeps its last good state, as
  the parent's does.
- **Config.** A new optional `subagents` block holds `models`, a list of
  entries with `provider`, `model` and an optional `effort`. Each entry must
  name a declared provider and one of its models; `effort` is checked
  against that model's efforts, and defaults to the parent's, or the
  nearest level above it that the model has (see `provider-config.md`). At startup the key of every provider used by
  the selection or a subagent entry is resolved, before the TUI starts.
  Without the block, children can only use the parent's model.
- **TUI buffers.** The TUI keeps one transcript per agent and shows one at a
  time, full screen. A minimal ex command line handles `:ls` and `:b N`;
  `gf` on a child's status line in the parent's transcript opens its buffer.
  The parent is buffer 0: `:ls` lists it first, then every child by number,
  each with its model and its state in the status line's words (waiting,
  running, finished, failed, cancelled), and `:b 0` returns to it.
  The status bar shows the shown agent's model, effort and state. The input
  sends to the shown agent; Enter does nothing while that agent is running.
  Ctrl-C cancels the shown agent's turn.
- **Turns the user starts on a child** run like any other run of that
  child: they take one of the five slots, waiting if none is free, and add
  a new status line to the parent's transcript.

## Testing Decisions

- Tests drive behaviour through public entry points and check what the model
  is sent and what the transcript shows, not how children are scheduled.
- **Core, through `RunTurn`,** with the existing mock `Model`, `Tool` and
  `Transcript`: a fresh child's request (system prompt, task, its own tools);
  the parent receiving `[agent N]` and the reply; the default and a chosen
  model; a fork's request matching the parent's prefix; a fork's refused
  `agent` call; the rejected combinations; resuming, and resuming a busy
  child; tool calls overlapping in time with results in call order; skill
  calls first, with dedup intact; the cap of five; a child's failure and the
  call cap; cancelling the parent and cancelling one child.
- **Config, through `Load`:** the `subagents` block resolved in order; an
  unknown provider, model or effort rejected; listed providers' keys
  resolved.
- **TUI, through the model's `Update`:** child events in their own buffer;
  the status line in the parent's; `:ls`, `:b N` and `gf`; input reaching
  the shown agent; Enter ignored while it runs; Ctrl-C in a child's buffer
  cancelling only that child.
- Prior art: the agent tests' mock ports and turn-level assertions, the
  skills tests for dedup within a response, the config tests for provider
  validation, and the TUI test that drives Ctrl-C through `Update`.

## Open Questions

None.

## Out of Scope

- Steering a running agent mid-turn, parent or child (wishlist #11).
- Children starting children.
- Named agent types with their own prompts and tools.
- Children running in the background while the parent goes on (wishlist #12).
- Saving children to disk and resuming them in a later session.
- Splits; buffers are full screen.
- Configuring a child's tool list in the config file.
- Forking onto a model other than the parent's.

## Further Notes

- The separate subagent tool list and fork's use of the parent's tools are
  deliberate: a fresh child never shares the parent's cache anyway, while a
  fork only stays cheap, and only keeps its thinking, if its system prompt,
  tools and model match the parent's exactly.
- Today the core runs tool calls one at a time and passes each call the
  results of earlier calls in the same response; skill dedup relies on that.
  Running calls at once keeps that order for skill calls only.
- Today only the selected provider's key is resolved, because key commands
  can prompt in the terminal. Subagent providers' keys follow the same rule
  by being resolved at startup too.
- `internal/adapters/tools` and `internal/adapters/skills` start commands in
  a new session, so concurrent shell calls can't fight over the terminal.
