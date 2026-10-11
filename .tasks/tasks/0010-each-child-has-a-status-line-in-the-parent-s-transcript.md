---
id: 10
title: Each child has a status line in the parent's transcript
slice: 1
status: done
depends_on: [4, 7]
---

**What.** Render each child's lifecycle events as one line in the parent's
transcript, updated in place: number, model, tool count and state, plus
whatever #4 decided for waiting children and for the `agent` call's own
entries.

**Done when.**
- [x] A TUI test feeds a child's started, progress and finished events and sees one line that ends as `agent N · <model> · <count> tools · finished`.
- [x] Failed and cancelled children show that state on the same line.
- [x] The waiting state and the `agent` call entries follow the spec as #4 left it.
