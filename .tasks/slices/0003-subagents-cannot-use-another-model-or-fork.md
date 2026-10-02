---
id: 3
title: Subagents cannot use another model or fork
status: open
---

## Goal

Let a subagent run on any model the user allows, on any configured provider,
and let the model fork a child from the current conversation. De-risks
running several providers in one session (keys resolved up front, one
`Model` per allowed entry) and fork keeping the parent's cache and thinking
valid.

## Demo

With a `subagents` block allowing Ollama `glm-5.3-flash:cloud`, asking for a
subagent on glm-flash runs one whose status line and buffer show glm-flash. A
fork on the parent's model starts from the parent's conversation, and its
first request reads the parent's cached prefix. `fork: true` with glm-flash
is rejected, and a `subagents` entry naming a model the provider doesn't
declare stops grimalkin at startup with a config error.

## Implements

- Subagents spec, Implementation Decisions: the `model` and `fork`
  parameters and their rejected combinations, forks, config, and the port
  giving the core a `Model` per allowed entry.
- Subagents spec, Solution: mixed fleet.

## Out of scope

- Forking onto a model other than the parent's.
- Choosing a child's tools in the config file.
- Named agent types with their own prompts and tools.

## Open in this slice

None. The spec settles the config shape, the parameter rules and how a fork
is built.

## Tasks

None yet. Run `to-tasks`.
