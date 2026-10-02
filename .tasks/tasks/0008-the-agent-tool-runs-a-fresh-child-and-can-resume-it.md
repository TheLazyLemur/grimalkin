---
id: 8
title: The agent tool runs a fresh child and can resume it
slice: 1
status: todo
depends_on: [7]
---

**What.** The core offers the `agent` tool with `task` and `agent` (resume)
as the subagents spec describes: a fresh child with the base prompt, the
subagent note, the skill catalogue and its own tool list, running its own
turn on the parent's model, numbered per session and kept for the session.
`model` and `fork` come in #3.

**Done when.**
- [ ] Through `RunTurn`, a call with a task makes the child's first request carry the base prompt with the subagent note, the task as its only message, and the subagent tool list, with no `agent` tool.
- [ ] The parent receives `[agent N]` followed by the child's final reply, under the 30 KB cap.
- [ ] Resuming with `agent: N` sends the task as a follow-up in that child's existing history.
- [ ] Resuming an unknown number, or a child that is running, returns an error result to the parent.
- [ ] A child's failed turn returns its reason to the parent as an error result.
- [ ] Cancelling the parent's turn cancels its running children and discards the turn.
- [ ] Each child's events report its number, and its start and end emit lifecycle events.
