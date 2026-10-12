---
id: 20
title: The agent tool takes `model`
slice: 3
status: done
depends_on: []
---

**What.** The core accepts a `Model` for each allowed subagent model, keyed by
`provider/model`, alongside the parent's, and the `agent` tool gains the
`model` parameter the subagents spec describes. The composition root fills the
list in #22; until then it is empty and children run on the parent's model.

**Done when.**
- [x] Through `RunTurn`, with allowed models wired, the `agent` tool's schema offers `model` as an enum of exactly those `provider/model` names.
- [x] A call with `model` sends the child's requests to that entry's `Model` and none to the parent's.
- [x] A call without `model` sends the child's requests to the parent's `Model`.
- [x] The child's waiting and started events name the model it runs on.
- [x] A call with both `agent` and `model` returns an error result and starts no turn.
- [x] A child started on another model and later resumed with `agent: N` runs on that same model.
- [x] With no allowed models wired, the tool's schema has no `model` parameter and a fresh child runs on the parent's model.
