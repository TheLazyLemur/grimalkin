---
id: 9
title: At most five children run at once
slice: 1
status: todo
depends_on: [6, 8]
---

**What.** Cap running children at five across the session. Further `agent`
calls wait for a slot instead of failing.

**Done when.**
- [ ] A core test with six `agent` calls in one response never has more than five children running at once, and all six results come back in call order.
- [ ] A waiting child emits whatever waiting state #4 decided.
