---
name: to-slice
description: "Write a slice into this repo's `.tasks/` markdown tracker from what has already been discussed: one outcome you can demo, what it builds, what it leaves open. Use whenever the user is deciding what to build next, says 'write that up as a slice', 'what's the next slice', 'let's do X next', or has just finished talking through a chunk of work that needs capturing in the tracker. Synthesises the conversation rather than interviewing. Not for breaking a slice into tasks (that is to-tasks) and not for a change that needs a full design first (that is tech-spec)."
disable-model-invocation: true
---

# To Slice

Turn what has been discussed into a slice. Do not interview the user;
synthesise what you already know.

The terms are in `CONTEXT.md`. This skill is only the part that is not written
down there: how to decide what belongs in a slice, and how to tell whether it is
any good.

## Where slices live

Slices and tasks are markdown files under `.tasks/`. If the user asks for a
different format or tracker, use that instead; the rest of this skill still
applies.

```
.tasks/
  slices/0020-bitbucket-pull-requests-cannot-be-reviewed.md
  tasks/0021-bitbucket-codehost-reads-pull-requests.md
```

Slices and tasks share one id sequence: the next id is one more than the
highest id anywhere under `.tasks/`. The file name is the id, zero-padded to
four digits, then the title as a slug.

## Create only the slice

Create the slice file. Do not edit other slices, tasks, ADRs or `CONTEXT.md`,
however obviously they seem to need it. Say what else you think should change
and let the user decide.

## The slice is thin on purpose

The design lives elsewhere: the domain in `CONTEXT.md`, decisions in
`docs/adr/`, and a larger design in a spec under `.agents/specs/`. The slice
adds only which outcome gets built next, how you will know, and what is still
open.

Point at those documents; never restate them. If part of your draft would
repeat an ADR or a spec, replace it with the reference.

**Use `tech-spec` instead of this skill** when the slice crosses a boundary
nothing has designed yet: a new port, a new platform, a new contract. That
produces a spec, and the slice then points at it. Check before you write: if an
ADR or spec already covers the chunk, a new spec is ceremony.

## Process

1. **Read.** The conversation, anything the user referenced, the ADRs and
   `CONTEXT.md` terms the work touches, and the code it changes.

2. **Name the demo.** One sentence: what can be run at the end that could not be
   run before. If you cannot write it, this is not a slice: say so and stop.

   A demo names a command and its observable result.
   "`review-assist https://bitbucket.org/o/r/pull-requests/3` opens the pull
   request and shows its diff" is a demo. "Bitbucket works" is not. A demo that
   includes the negative case is usually stronger: showing the tool refusing bad
   input proves more than showing the happy path.

3. **Sketch the test seams, and check them with the user before writing.** Where
   will this be tested, at what level? Prefer seams that already exist, and the
   highest one that still catches the bug. Fewer seams is better.

   This step is easy to skip and worth keeping. It is where you find out the
   slice is bigger than it looked.

4. **Create the slice file.** Fill every part:

   ```markdown
   ---
   id: 20
   title: Bitbucket pull requests cannot be reviewed
   status: open
   ---

   ## Goal
   ...

   ## Demo
   ...

   ## Implements
   - ADR 0002: ...

   ## Out of scope
   - ... (goes to #n)

   ## Open in this slice
   - ... Recorded in: a new ADR

   ## Tasks
   None yet. Run `to-tasks`.
   ```

   - **Goal**: why this slice exists, and what it de-risks.
   - **Demo**: from step 2.
   - **Implements**: the ADRs, spec sections or `CONTEXT.md` terms it builds,
     one line each.
   - **Out of scope**: what a reader would expect here but gets elsewhere. Name
     the slice or task it goes to.
   - **Open in this slice**: decisions this slice must make that nothing has
     settled. **Each names where its answer will be recorded**: an ADR,
     `CONTEXT.md` or a spec. Never the slice itself: a decision buried in a
     finished slice is not found again.

   The title is a claim: what is missing or wrong today. Avoid file paths and
   code snippets; they go stale. A type shape that pins a decision is the
   exception.

5. **Stop.** Do not create tasks. Tell the user to run `to-tasks` when ready.
