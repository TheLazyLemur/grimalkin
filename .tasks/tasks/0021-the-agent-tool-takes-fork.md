---
id: 21
title: The agent tool takes `fork`
slice: 3
status: done
depends_on: [20]
---

**What.** The `agent` tool gains `fork`: a child that starts from the parent's
conversation, as the subagents spec's Forks decision describes.

The spec leaves one thing unsettled, which this task decides first. A fork's
history runs "up to the response being handled" and is followed by the task as
a user message, but that response holds the `agent` tool call itself, and the
API rejects a tool call that has no result before the next user message.
Decide how the fork's history ends so the request is valid and the parent's
cached prefix is still read.

**Done when.**
- [x] The subagents spec states how a fork's history ends: what stands in for the results of the tool calls in the response being handled, and where the task goes.
- [x] Through `RunTurn`, a fork's first request has the parent's system prompt and the parent's tool list, `agent` included, unchanged.
- [x] That request's messages begin with the parent's history up to the response being handled, each message's content identical to the parent's, and end as the spec states.
- [x] The parent's own history is unchanged by the fork, and its next request is what it would have been with a fresh child.
- [x] An `agent` call made by a fork returns the error "subagents can't start subagents" to the fork and starts nothing.
- [x] A call with `fork: true` and a `model` other than the parent's returns an error result and starts no turn.
- [x] A call with `fork: true` and the parent's own model as `model` starts a fork.
- [x] A call with both `agent` and `fork` returns an error result and starts no turn.
- [x] A fork counts towards the cap of five and has a status line, like any child.
