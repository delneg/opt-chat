# Measuring the chat's memory, 17 October 2026

Agent-memory papers and benchmarks in 2026 point the same way:

- Keep the raw text and let the model search it. [HyMem](https://arxiv.org/abs/2602.13933) lost 17 points on LoCoMo without its raw-text level.
- Plain BM25 holds up well. On [MemoryAgentBench](https://github.com/HUST-AI-HYZ/MemoryAgentBench) it beat the memory products at retrieval.
- A stable summary prefix works with the prompt cache. [Mastra's observational memory](https://mastra.ai/research/observational-memory) scored 94.9 on LongMemEval.

opt-chat already does all three: the view, recall and `zoom`/`search`. So the first job was to measure it, on the user's own chat rather than on a public benchmark.

## How

1. **Questions.** `eval/gen.py` samples 60 messages from a copy of the chat: Opt's replies, the user's messages, agent reports, tool results and imported notes. For each one, opus writes a question the user might ask weeks later and the gold answer, both taken from that message alone. 56 of the 60 were kept.
   - Ten more questions ask about things the chat never held. A grep checks each one. The right answer to these is "no record".
2. **Answers.** `opt-chat -eval questions.jsonl -eval-out answers.jsonl` asks each question as a side question (`/btw`).
   - Each call gets the same view, recall and tools as a turn, and no earlier side questions.
   - Nothing joins the chat.
   - It records each answer, the tools it called, its cost and its time.
3. **Grades.** `eval/judge.py` grades each answer against its gold with opus: 1 right, 0.5 partly, 0 wrong.
4. **Recall offline.** `eval/recall_rank.py` re-ranks with recall's scoring under other settings, with no model call. It checks two sets:
   - the generated questions;
   - 13 real lookups from the chat's history: a user message, then Opt's `zoom(id, 1)` into a message 20 or more back.

All runs used opus at high effort with `-claude-config=false`, on a copy of a chat of 29,704 messages. One run of 66 questions costs about $5.50 to $7.30 at API list prices.

## Results

| run | facts | wrong | tool calls / question | answered without tools | $ / question | s / question |
|---|---|---|---|---|---|---|
| no recall | 0.955 | 1 | 3.77 | 2 | 0.110 | 19 |
| recall, b 0.75 (three runs) | 0.90–0.95 | 1–2 | 2.00–2.08 | 25–28 | 0.085–0.087 | 15–17 |
| **recall, b 0.3** | **0.955** | **0** | **1.20** | **36** | **0.083** | **14** |
| recall, b 0.3, 2,400-byte excerpts | 0.973 | 0 | 1.30 | 41 | 0.091 | 14 |
| recall, b 0.3, search falls back to words | 0.938 | 0 | 1.23 | 37 | 0.084 | 15 |
| no recall, search falls back to words | 0.902 | 3 | 3.64 | 2 | 0.110 | 22 |

Every run got all ten "never discussed" questions right.

- **Accuracy is near the ceiling.** With `zoom` and `search`, opus finds almost every fact, with recall or without it. The three runs of the same setting differ by up to 0.04, so smaller differences are noise.
  - Most answers graded 0.5 were in fact right. The chat later changed the fact: a correction from the user, or a flag that was switched on. Opt gave the later version, which the prompt asks for ("its latest word on a thing is the truth").
- **Recall saves work.** Recall at b 0.75 cut tool calls by 45%, cost by 22% and time by 21%, against no recall.
- **b 0.3 saves more.**
  - BM25's usual length scaling (b 0.75) pushed long messages down: Opt's longer replies and agent reports, where many answers are.
  - At b 0.3 the message holding the answer was among the 8 recalled for 40 of 56 questions, against 32. On the real lookups, MRR went from 0.206 to 0.234.
  - Live, tool calls fell a further 40% (2.04 to 1.20 on average). Answers with no tool call rose from about 27 to 36 of 56. Accuracy was no worse.
  - The case the length scaling was added for (the [notes of 14 October](2026-10-14-borrowed.md), a question whose one rare word is in two short messages) still ranks those two first.
  - `recall` now uses b 0.3.

## Tried and dropped

- **Longer excerpts (2,400 bytes instead of 1,200).** Accuracy was within noise. Cost rose 10%.
- **Search falling back to words.** A third of the chat's searches with no recall found nothing: they were phrases that the chat had put in other words. The fallback ranked the messages that share the phrase's words, as recall does. It did not reduce tool calls, with recall (1.23 against 1.20) or without it (3.64 against 3.77).
- **A warning against misleading memory**, after [MemTrapBench](https://arxiv.org/html/2608.20202), where every memory method scored about 15 points below no memory at all. Opt said "no record" to every question about things the chat never held, so there was nothing for the warning to fix here.

## Limits

- **The questions are easy for search.** They come from one message each, and opus wrote them knowing the message, so they share its words more than a user's later question would.
  - The real lookups share fewer: recall put the target among its first 8 in only 4 of 13.
  - Questions that need several messages, or the order of events, were not tested.
- **The judge is opus**, and the gold answers can be out of date. Read the 0s and the 0.5s.
- **The questions hold the chat's facts.** They stay out of the repo.
