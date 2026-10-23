# Memory summary calls without tools

Written 2026-10-24, after a summary invented a task from a tool reminder.

## What went wrong

A compaction (a call that writes a memory summary line) runs Claude Code
with the turns' tool list, so that on the turns' model its prompt prefix
matches the turns' and reads from their cache. Listed tools bring Claude
Code's reminders for them. Bash's reminder gives the commit attribution to
use, and it names the model of the call. A compaction on a different model
from the turns (the recipe's `haiku`) sees a different model name than the
chat's commits carry. One summary then wrote that the chat's commits
credited the wrong model and should be fixed, and later lines copied that
as an open task for weeks.

The cache doesn't pay for the tools here: a cache entry is per model, so a
compaction on another model never reads the turns' prefix anyway.

## What changed

`-compact-bare` (on): a compaction that runs on a different model from the
turns gets no tools (`--tools ""`), so it gets no tool reminders. On the turns' model it
keeps them, and the cache it shares with turns. `-compact-bare=false` keeps
the old behaviour.

## How it was checked

`TestCompact` checks that a compaction on another model gets `--tools ""`
and keeps its model. It fails without the change, with the tools listed.

In a fork, this went in with other changes (a stricter compaction task and
a check for copied lists). Together they took the invented claim from 14
of 20 runs to 0 of 20 on the two messages where it first appeared. How much
of that is this change alone was not measured.
