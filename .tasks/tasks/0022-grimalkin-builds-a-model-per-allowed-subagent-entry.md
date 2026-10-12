---
id: 22
title: grimalkin builds a `Model` per allowed subagent entry
slice: 3
status: done
depends_on: [19, 20]
---

**What.** The composition root builds an adapter for each entry of the
config's `subagents` block and hands them to the core keyed by
`provider/model`, so the slice's demo runs end to end.

**Done when.**
- [x] With a `subagents` block allowing Ollama `glm-5.3-flash:cloud`, asking grimalkin for a subagent on glm-flash shows a status line naming `ollama/glm-5.3-flash:cloud`, and the parent answers from its reply.
- [x] That child's requests go to the Ollama provider's base URL with that provider's key and the entry's effort.
- [x] An entry whose provider's wire has no adapter stops grimalkin at startup with an error naming the entry, before the TUI starts.
- [x] A `subagents` entry naming a model its provider does not declare stops grimalkin at startup with the config error, before the TUI starts.
- [x] With no `subagents` block, grimalkin starts and subagents run on the parent's model, as before.
- [x] `fork: true` with glm-flash comes back to the model as an error.
