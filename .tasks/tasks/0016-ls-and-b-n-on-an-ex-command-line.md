---
id: 16
title: "`:ls` and `:b N` on an ex command line"
slice: 2
status: done
depends_on: [12, 15]
---

**What.** A minimal ex command line in the TUI, opened with `:` from the
transcript, handling `:ls` and `:b N` and nothing else. What `:ls` lists, how
it marks each child and how the parent's buffer is reached are as the
subagents spec states after #12.

**Done when.**
- [x] `:` in the transcript opens a command line; Esc leaves it with nothing done, and Enter runs it.
- [x] `:ls` shows the buffers the spec says it lists, each with its number, and each child marked by state as the spec says.
- [x] `:b N` shows child N's buffer, for a running child and for one that has ended.
- [x] The command the spec gives for the parent shows the parent's buffer again.
- [x] `:b` with a number no buffer has, and any other command, shows an error in the status bar and changes nothing else.
- [x] `:` typed in the input while inserting is still an ordinary character.
