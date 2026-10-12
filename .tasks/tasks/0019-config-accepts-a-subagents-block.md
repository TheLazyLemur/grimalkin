---
id: 19
title: Config accepts a `subagents` block
slice: 3
status: done
depends_on: []
---

**What.** The config file takes the optional `subagents` block the subagents
spec's Config decision describes, and `Load` returns the allowed subagent
models resolved, each with what is needed to build its adapter.

```json
"subagents": {
  "models": [
    { "provider": "ollama", "model": "glm-5.3-flash:cloud", "effort": "low" }
  ]
}
```

**Done when.**
- [x] Through `Load`, a `subagents` block returns its entries in the order written, each with its provider, wire, base URL, key, model, thinking and effort.
- [x] An entry naming a provider that is not declared is a config error naming the entry.
- [x] An entry naming a model its provider does not declare is a config error naming the entry.
- [x] An entry whose `effort` is not one of that model's efforts is a config error naming the entry.
- [x] The key command of every provider a subagent entry uses is run by `Load`, and one that fails is a config error.
- [x] A provider used by neither the selection nor a subagent entry still has no key command run.
- [x] A config with no `subagents` block loads as before, with no subagent models.
- [x] The provider-config spec describes the block.
