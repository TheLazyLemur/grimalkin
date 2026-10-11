---
id: 5
title: Tool output appears under the command that produced it
slice: 1
status: in-progress
depends_on: []
---

**What.** The TUI adds a tool's output as the next entry after whatever came
last, which only works while calls run one at a time. Place each finished
tool's output under the command entry with the same call ID, so that output
from calls running at once lands under the right command.

**Done when.**
- [ ] A TUI test sends two tool-started events, then the second call's finished event, then the first's, and each output appears directly under its own command.
- [ ] An error result is placed the same way.
- [ ] Existing transcript tests pass unchanged.
