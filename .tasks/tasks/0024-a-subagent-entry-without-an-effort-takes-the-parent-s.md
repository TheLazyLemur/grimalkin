---
id: 24
title: A subagent entry without an effort takes the parent's
slice: 3
status: todo
depends_on: [19]
---

**What.** A `subagents` entry that names no `effort` resolves to the parent's
effort, the top-level `effort` of the config. Today it resolves to none, and
the child's model uses its own default. An entry that names an `effort` keeps
it, as now.

When the child's model does not have the parent's effort, the entry takes the
next higher one it does have. "Higher" is read from the parent's model's own
`efforts` list, which is written lowest first: walk up that list from the
parent's effort and take the first level the child's model also lists. If
none of the higher levels is listed, take the last level in the child's
model's own list, its highest.

**Done when.**
- [ ] Through `Load`, an entry with no `effort` on a model that lists the parent's effort resolves to the parent's effort.
- [ ] An entry with an `effort` resolves to that effort, whatever the parent's is.
- [ ] With the parent at `xhigh` on a model listing low, medium, high, xhigh, max, an entry with no `effort` on a model listing low, high, max resolves to `max`.
- [ ] With the parent at `max`, an entry with no `effort` on a model listing low, medium, high resolves to `high`.
- [ ] An entry with no `effort` on a model with no effort levels resolves to no effort, and is not an error.
- [ ] The provider-config spec states the rule, with the order it reads "higher" from.
- [ ] The subagents spec's Config decision says an entry's `effort` defaults to the parent's.
- [ ] In grimalkin, with the glm-flash entry's `effort` removed and the parent at high, the startup line and the status bar in a glm-flash child's buffer both show effort high.
