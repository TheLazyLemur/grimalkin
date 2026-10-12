---
id: 12
title: Decide what `:ls` lists and how ended children are marked
slice: 2
status: done
depends_on: []
---

**What.** Settle what slice #2 leaves open about buffers: whether `:ls` lists
the parent, how a cancelled or failed child is marked there, and how the user
gets back to the parent's buffer. Settle too what the spec does not say about
a turn the user starts on a child: whether it takes one of the five slots, and
whether it gets a status line in the parent's transcript.

**Done when.**
- [x] The subagents spec states whether `:ls` lists the parent.
- [x] The subagents spec states how `:ls` marks a running, finished, failed and cancelled child.
- [x] The subagents spec states which command returns to the parent's buffer.
- [x] The subagents spec states whether a turn the user starts on a child counts towards the cap of five.
- [x] The subagents spec states whether a turn the user starts on a child adds a status line to the parent's transcript.
- [x] The `:ls` question is removed from the spec's Open Questions and from slice #2's Open in this slice.
