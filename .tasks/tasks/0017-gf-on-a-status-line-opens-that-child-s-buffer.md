---
id: 17
title: "`gf` on a status line opens that child's buffer"
slice: 2
status: done
depends_on: [15]
---

**What.** In the parent's transcript, `gf` with the cursor on a child's status
line shows that child's buffer.

**Done when.**
- [x] `gf` on the status line of child N shows child N's buffer, whether the child is waiting, running or ended.
- [x] When a child has several status lines from several runs, `gf` on any of them shows the same buffer.
- [x] `gf` on any other line changes nothing.
- [x] `g` followed by the keys that already use it (such as `gg`) behave as before.
