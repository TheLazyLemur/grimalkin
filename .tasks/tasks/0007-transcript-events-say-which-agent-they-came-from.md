---
id: 7
title: Transcript events say which agent they came from
slice: 1
status: in-progress
depends_on: []
---

**What.** Every transcript event carries the number of the agent it belongs
to, the parent being 0, and the core gains the child lifecycle events from
the subagents spec (started with number and model, progress with tool count,
finished, failed, cancelled). Until buffers exist (#2), the TUI shows only
agent 0's turn events.

**Done when.**
- [ ] Every event the core emits for the parent's turn reports agent 0, checked through `RunTurn`.
- [ ] The lifecycle event types exist, carrying the fields the subagents spec names.
- [ ] A TUI test shows that a turn event from an agent other than 0 does not appear in the transcript.
- [ ] Existing core and TUI tests pass.
