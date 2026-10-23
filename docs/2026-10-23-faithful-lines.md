# Lines that say only what their input says

As of 2026-10-23.

## What went wrong

A summary line claimed a commit credited the wrong model and should be
fixed. The commit's message said no such thing; nobody had asked. Later
lines copied the claim into their lists of owed items, Opt read those lines
as the chat's word, and the item came back for a week after it was dropped.

Three causes, each checked:

1. **Claude Code's reminders.** A compaction lists the turns' tools, as the
   recipe says, so that on one model it reads their prefix from the cache.
   With Bash listed, Claude Code adds a reminder to end commits with a
   co-author line naming the call's own model. A compaction on another model
   than turns read that as the chat's rule, and judged each commit in its
   input by it. On another model the cache isn't shared anyway, so the tools
   buy nothing.
2. **Owed lists in memory.** Lines carried lists of owed and open items.
   A compaction of a message that had nothing to do with them took the list
   from its context and wrote it into its own line, item for item.
3. **The task allowed it.** It said context is "never to add what <input>
   lacks", but named none of the shapes the additions took.

## What changed

- `-compact-bare` (on): a compaction on another model than turns runs with
  no tools, so it gets no tool reminders. On the turns' model it keeps them,
  and the cache it shares with turns.
- The compaction task names what context must not add: owed or open lists,
  verdicts its input doesn't make ("wrong", "should be"), outcomes, news of
  other work. It says Claude Code's reminders to the call are not the chat's.
- `Compact` checks a line for the head of an owed or open list its input
  lacks, and asks once for the line again without it, as it does for a line
  too long.
- `-owed` (on): an owed list in `<dir>/owed.jsonl`, outside memory. Opt adds
  an item with the `owe` tool, which must cite the user's message that asked
  for it; each turn opens with the list; only the user closes an item, with
  `/done N` (`/owed` lists them). Opt has no tool to close one.
- `-redo REGEXP`: at start, each message's line that matches while its
  message doesn't is marked to build again, with the lines built on it,
  through the same marks a line answered "ok" gets.
- `-eval-compact FILE`: builds named lines again as a compaction would now,
  without keeping them, and writes them next to the stored line and the
  input. `eval/faithful.py` asks a judge model which claims of each line
  its input doesn't support.

## Measured

On a real chat (data kept out of the repo), Haiku compactions, 10 runs each
of the two messages where the claim first appeared:

| | lines with the claim | lines adding an owed list | lines adding a verdict | claims the judge found unsupported |
|---|---|---|---|---|
| before | 14/20 | 8/20 | 10/20 | 62 |
| task text only | 9/20 | 5/20 | 9/20 | 44 |
| no tools only | 11/20 | 9/20 | 8/20 | 51 |
| no tools and task text | 1/20 | 3/20 | 2/20 | 29 |
| all, with the owed check | 0/20 | 0/20 | 0/20 | 22 |
| all, on Sonnet | 4/20 | 0/20 | 8/20 | 48 |

Sonnet follows its context more closely, false items in it too, and costs
more per token: the fix is the task and the call, not the model.

A sample of 50 messages built again, one run each: before, 17 lines with
the claim their message lacked and 14 with an owed list from context;
after, 1 and 0. The judge found 119 unsupported claims before, 53 after.
The judge is noisy: many of the 53 are references the context resolved.
