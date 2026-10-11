---
id: 6
title: A response's tool calls run at the same time
slice: 1
status: done
depends_on: [5]
---

**What.** The core runs a response's tool calls one after another. Run them
at the same time, shell included, and send the results back in call order.
Skill tool calls run first and one at a time, so a skill loaded twice in one
response still gets the "already loaded" note.

**Done when.**
- [x] A core test with two tools that each block until the other has started finishes, and both results go back in call order.
- [x] The existing test for a skill loaded twice in one response still passes, with another tool call in the same response.
- [x] Cancelling during concurrent calls stops them all and discards the turn, checked through `RunTurn`.
