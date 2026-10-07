---
name: refine-task
description: "Take a task in this repo's `.tasks/` markdown tracker to ready: rewrite vague acceptance criteria so someone other than the author can check them, confirm the parent slice's demo is concrete, then mark it ready. Use whenever the user asks if a task is ready or well-specified, calls a Done when vague, woolly or unclear, wants a task groomed, refined or triaged before picking it up, or is about to start work on a task still at todo. Fixes what it finds rather than only reporting it."
---

# Refine Task

Take one task to ready. Refining means fixing what is vague, not naming it.

This skill is how to tell a real acceptance criterion from a wish. The terms
are in `CONTEXT.md`. Tasks and slices are markdown files under `.tasks/`, in
the format `to-tasks` describes. If the user asks for a different format or
tracker, use that instead; the rest of this skill still applies.

## Write only what the task needs

Fix the task, and the parent slice's demo if that is what blocks it. Do not
touch other tasks or slices. Say what else needs changing and let the user
decide.

## Ready

Both must hold:

1. **The parent slice's demo is concrete**, where the task has a parent. A task
   cannot be clearer than the outcome it serves.
2. **The task's own acceptance criteria are checkable**: someone who did not
   write them can tell whether each is met, without asking you.

Ready is about the writing, not the graph. A task can be ready and still wait on
a dependency; the two are reported separately.

An unplanned task has no `slice`, so only the second test applies. Do not
invent a slice to give it one.

## The checkable test

For each criterion ask: **what would I run, read or look at to decide this is
done?** No answer means it is not a criterion yet.

- "`go test ./...` passes with a test that posts to a fake code host" is
  checkable.
- "`review-assist` refuses to start when the config file is readable by other
  users, and names the file" is checkable.
- "The Bitbucket adapter works" is not checkable. Ask what it would do that
  proves it.
- "Decided, and the docs are updated" is not checkable. It joins two things
  with "and", and neither is observable. Split it, and name where the decision
  is recorded.

One criterion per checkbox. Split any that join two observable things.

A criterion that describes a decision is fine, but it must name where the
answer lands: an ADR in `docs/adr/`, `CONTEXT.md`, or a spec. A decision that
lives only in someone's head is not done.

## Process

1. **Gather the task, its slice's demo and its dependency state.** Read the task
   file, the slice file it names, and the `status` of each task in its
   `depends_on`.

   Scan the criteria for a vague word ("works", "properly", "handles",
   "supports", "clean"), for two things joined with "and", and for criteria
   naming nothing to run or read. These are prompts, not verdicts: a flagged
   criterion may be fine, and an unflagged one may still be useless, so apply
   the checkable test to every one. Then read anything the task or slice cites
   (ADRs, specs, `CONTEXT.md`), and check they still say what the task assumes.
   Read the code the task touches: a criterion that names behaviour the code
   already has, or cannot have, is wrong.

2. **Deal with the slice first if its demo is vague.** A vague demo makes every
   task under it unrefinable, and refining them one at a time hides that.

   Usually you can fix it: the concrete version is often already in an ADR, a
   spec or the conversation, and carrying it across is a repair, not a scope
   change. Do that, say plainly that you did, and carry on to the task.

   Stop and ask only when the demo is genuinely undecided rather than merely
   unwritten: when settling it would change what the slice covers. That is the
   user's call.

3. **Fix the task.** Rewrite the body so it has a **What.** line and a
   **Done when.** checklist, with each criterion passing the checkable test.
   Keep what the body already says about what breaks and how we know it; that
   is what lets someone pick the task up cold. Remove stale file paths. Leave
   the frontmatter alone except as below.

   Check the dependencies against what the task actually needs. If an edge is
   plainly wrong, correct `depends_on`, say what you changed and why, and
   resync the parent slice's Tasks section as `to-tasks` describes. If it is a
   judgement call, leave it and raise it.

4. **Ask only what you cannot resolve.** Most vagueness is answerable from the
   slice, the ADRs and the code. Go there first. Bring one short list of what
   genuinely needs the user's judgement.

5. **Mark it ready** by setting `status: ready`, then say whether it is also
   unblocked, and if not, which dependencies are not yet `done`.

## Refining a whole slice

Check the slice's demo once, then work the tasks one at a time. Report which are
ready and which are still open on a question, rather than lowering the bar to
finish the set.
