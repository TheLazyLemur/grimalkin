---
id: 14
title: The user can run a turn on a finished child
slice: 2
status: done
depends_on: [13]
---

**What.** The core lets the user, not only the parent's `agent` call, run a
turn on a child by number: the follow-up typed into a child's buffer. The turn
is the child's own, in its own history, with its events under its number.
Whether it takes a slot or adds a status line to the parent's transcript is
what #12 decides; this task follows the spec as it then stands.

**Done when.**
- [x] A user turn on finished child N sends the prompt as a follow-up after that child's existing history, with the child's system prompt and tool list.
- [x] The turn's events are shown under agent N, not under the parent.
- [x] A user turn on a child that is running, by the parent's call or the user's, does not start.
- [x] While a user turn runs on child N, the parent's `agent: N` call returns the busy error.
- [x] After a user turn ends, the parent resuming child N sends a history that includes the user's follow-up and the child's reply.
- [x] Cancelling child N during a user turn discards that turn and leaves the child's history as it was before it.
- [x] A user turn that fails is discarded from the child's history, with the reason shown in the child's transcript.
