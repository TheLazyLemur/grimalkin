---
id: 18
title: Input and Ctrl-C act on the shown agent
slice: 2
status: done
depends_on: [13, 14, 15]
---

**What.** The input, Enter, Ctrl-C and the status bar's state follow the
buffer on screen, as the subagents spec's TUI buffers decision describes. The
status bar's model and effort still show the parent's until #23.

**Done when.**
- [x] With a finished child's buffer shown, Enter sends the input to that child: the prompt and the child's answer appear in its buffer, and nothing is added to the parent's conversation.
- [x] With a running child's buffer shown, Enter does nothing and the input keeps its text.
- [x] With a running child's buffer shown, Ctrl-C cancels that child only: the parent's turn goes on and other children keep running.
- [x] After that Ctrl-C the child's buffer is still there and shows its turn as cancelled.
- [x] With the parent's buffer shown, Ctrl-C during a turn still cancels the turn and every child.
- [x] With a child's buffer shown and nothing running anywhere, Ctrl-C quits, as it does in the parent's.
- [x] The status bar shows "working" while the shown agent runs and "ready" otherwise, whatever the other agents are doing.
- [x] With the parent's buffer shown while a child runs a turn the user started, Enter sends to the parent as usual.
