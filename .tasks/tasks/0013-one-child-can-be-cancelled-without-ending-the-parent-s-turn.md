---
id: 13
title: One child can be cancelled without ending the parent's turn
slice: 2
status: done
depends_on: []
---

**What.** The core lets one running child be cancelled by number, as the
subagents spec's Cancelling decision describes. Today the only cancel is the
parent's, which ends the whole turn and every child with it.

**Done when.**
- [x] Through `RunTurn`, cancelling child N while it runs returns `[agent N]` followed by "cancelled by user" to the parent as an error result.
- [x] The parent's turn goes on after that result: its next request carries it, and the turn is not discarded.
- [x] Another child running at the same time finishes and returns its reply.
- [x] The cancelled child's transcript shows its turn discarded, and the parent's shows the child as cancelled.
- [x] Resuming the cancelled child sends the follow-up after its history as it stood before the cancelled turn.
- [x] Cancelling a child that is waiting for a slot gives the same error result, and the child never starts.
- [x] Cancelling a child that is not running, or a number no child has, does nothing.
- [x] Cancelling the parent's turn still cancels every running child and discards the turn.
