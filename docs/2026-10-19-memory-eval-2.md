# Measuring the chat's memory, round 2, 19 October 2026

[Round 1](2026-10-17-memory-eval.md) asked questions that opus wrote while looking at one message each, so they reused that message's words and were easy for search. This round asks harder questions, of the kinds [LongMemEval](https://arxiv.org/abs/2410.10813) and [MemTrapBench](https://arxiv.org/html/2608.20202) test, about the same copy of the user's chat (now 29,841 messages).

## Questions

`eval/gen2.py` drew 135 questions, each with a gold answer and the ids of the messages that hold it:

| kind | n | what it asks |
|---|---|---|
| paraphrase | 23 | a fact from one message, worded without that message's rare words, names or numbers |
| multi | 30 | facts from two or more messages, from different days where possible |
| order | 24 | which came first, what came after, how long between, or when |
| update | 22 | a fact a later message changed; the gold is the latest value and the earlier one is a wrong answer |
| premise | 24 | a question built on a false premise close to the truth; a good answer corrects it |
| real | 12 | a user message after which Opt opened an old message, rewritten as the question the user was asking, in the user's words |

For the four kinds built on several messages, opus got a target message and the 8 messages that share its rarest words most, and wrote a question only when they supported one. `eval/judge.py` now grades the premise and update kinds by their own rules.

## Baseline

opus at high effort, recall 8, `-claude-config=false`, three runs:

| run | all | paraphrase | multi | order | update | premise | real | tool calls / q | $ / q |
|---|---|---|---|---|---|---|---|---|---|
| 1 | 0.874 | 0.87 | 0.78 | 0.96 | 0.84 | 0.92 | 0.92 | 3.36 | 0.175 |
| 2 | 0.870 | 0.96 | 0.73 | 0.94 | 0.80 | 0.92 | 0.96 | 3.59 | 0.183 |
| 3 | 0.889 | 0.91 | 0.75 | 0.96 | 0.91 | 0.94 | 0.92 | 3.63 | 0.182 |

These questions cost about twice as much as round 1's ($0.083) and take three times the tool calls (1.2).

Reading the answers that scored below 1 in two runs or more:

- **Search missed the chat's own words.** In 34 of 246 searches, the text searched for is in no message. Opt then tried other phrasings, one call each, and gave up on some questions. The worst case was a question in English about a message in another language: every English search came back empty.
- **Most update misses are stale golds.** A gold comes from the messages its generator saw, and the chat often changed the fact again later. Opt gave the later value, or checked the file on disk and gave the current one. It rarely gave the stale value.
- **Multi questions lose half points for completeness.** Opt finds most of the parts but not all.

## Recall, offline

Replays of recall's ranking under other settings, with no model call, as `eval/recall_rank.py` does, counted the questions whose evidence is among recall's first 8:

- **b 0.3 beats 0.75 on every kind**, as in round 1. On the real lookups, which `eval/recall_rank.py` now counts correctly (the model sends a zoom's id as a number or as a string, and the strings were skipped), b 0.3 put the target in the first 8 for 4 of 17, against 3.
- **Nothing tried closes the gap on paraphrased questions** (7 of 23 at b 0.3):
  - stripping word endings: 8 of 23, and worse elsewhere;
  - also matching the summary lines above each message in the tree (document expansion with the view's own summaries): 9 of 23 at best, and premise questions fell from 18 to 16 of 24;
  - 12 or 16 messages instead of 8: one or two more on multi, order and premise, none on paraphrase, at half again the input.

None of these went live.

## Live variants

| variant | runs | all | tool calls / q | $ / q | s / q |
|---|---|---|---|---|---|
| baseline | 3 | 0.870–0.889 (0.876) | 3.52 | 0.180 | 23 |
| **search falls back to words** | 3 | **0.904–0.907 (0.906)** | 3.47 | 0.179 | 21 |
| prompt: retry a search in other words and other languages | 1 | 0.900 | 4.10 | 0.187 | 25 |
| both | 1 | 0.896 | 3.44 | 0.177 | 22 |

- **The fallback.** When `search` finds no message that holds the text, it shows the 10 messages that best share its words, ranked as recall ranks them.
  - Over the three runs each, it scored 0.028 higher per question (95% bootstrap interval 0.007 to 0.053). It did better on 20 questions and worse on 9.
  - The gains are where the chat put things in other words: paraphrase +0.06, premise +0.06, multi +0.04. Order and update did not change.
  - It cost no more, and was two seconds faster per question.
  - Round 1 dropped it: on questions that reuse the message's words, a search seldom finds nothing, so it had little to do. `search` now falls back to words.
- **The prompt line** scored about the same with 16% more tool calls. Dropped.

## The research's other gaps

The [notes of 14 October](2026-10-14-borrowed.md) and the research before round 1 named five gaps. Measurement is now done. The data does not support the other four yet:

- **Memory traps.** Premise questions already scored 0.92–0.94, and 0.98 with the fallback. Opt corrects false premises; a warning in the prompt has nothing to fix.
- **Search by date.** Order questions scored 0.94–0.96 with `date` and `search` as they are.
- **Newer facts replacing older ones.** Opt gave the later value in almost every update question it missed; the golds were older than its answers.
- **A fixed format for subagent reports** (after [HyMem](https://arxiv.org/abs/2608.15703)). Questions whose answer is in a subagent's report scored 0.92 (n = 6). The chat holds 59 such reports in all.

## Limits

- One chat, one model (opus), one judge (opus). The golds can be out of date: read the 0s and the 0.5s.
- Questions built from related messages were found by word overlap, as recall finds them, which favors recall on those kinds.
- This round cost about $210 at API list prices: $194 for the eight runs, the rest for drawing the questions and grading.
- The questions hold the chat's facts. They stay out of the repo.
