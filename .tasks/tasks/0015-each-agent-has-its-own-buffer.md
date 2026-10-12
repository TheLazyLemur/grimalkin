---
id: 15
title: Each agent has its own buffer
slice: 2
status: done
depends_on: []
---

**What.** The TUI keeps one transcript per agent and shows one at a time, full
screen, as the subagents spec's TUI buffers decision describes. Today a
child's events are dropped and there is a single transcript. This task makes
the buffers and the notion of the shown one; the commands that switch between
them come in #16 and #17, so tests set the shown buffer directly.

**Done when.**
- [x] A child's events (reply text, tool calls and their output, a discarded turn) appear in that child's buffer and not in the parent's.
- [x] The parent's buffer still shows its own events and each child's status line, as before.
- [x] A child's buffer exists from its first event and is kept after the child finishes, fails or is cancelled.
- [x] With a child's buffer shown, the screen draws that child's transcript, and events arriving for it appear without a key press.
- [x] Events for an agent whose buffer is not shown do not change what is on screen.
- [x] Each buffer keeps its own cursor, scroll position and folds: after showing another buffer and coming back, they are where they were left.
- [x] With no children, the screen and every existing key behave as before.
