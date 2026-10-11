---
id: 11
title: grimalkin starts with subagents wired in
slice: 1
status: done
depends_on: [8]
---

**What.** The composition root builds the subagent tool list (the shell, plus
the skill tool when skills are loaded) and gives it to the core, so the
`agent` tool is on in a real session.

**Done when.**
- [x] Running grimalkin and asking it to use a subagent produces an `[agent 1]` result in a real session.
- [x] The README describes the `agent` tool, and CONTRIBUTING lists it with the core's tools.
- [x] Build, vet and tests pass.
