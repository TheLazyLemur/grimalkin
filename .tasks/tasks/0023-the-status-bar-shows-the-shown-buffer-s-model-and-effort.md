---
id: 23
title: The status bar shows the shown buffer's model and effort
slice: 3
status: done
depends_on: [15, 19]
---

**What.** The status bar's model and effort are those of the agent whose
buffer is on screen. Today the bar shows one fixed label, the parent's.

**Done when.**
- [x] With the buffer of a child on another allowed model shown, the status bar shows that model and that entry's effort.
- [x] With the buffer of a child on the parent's model shown, a fork included, the status bar shows the parent's model and effort.
- [x] Back in the parent's buffer, the status bar shows the parent's model and effort.
- [x] A subagent entry with no effort shows as the parent's label does for a model with none.
