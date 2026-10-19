"""Grade -eval's answers against the gold answers with opus (1 right, 0.5
partly, 0 wrong), keep the grades next to them, and print a summary.

    uv run --no-project python eval/judge.py answers.jsonl [more.jsonl ...]

Read the 0s and 0.5s: a gold answer drawn from one message can be out of
date when the chat later changed the fact, and then the answer is right.
"""
import json, os, subprocess, sys
from concurrent.futures import ThreadPoolExecutor

SYSTEM = """You grade an AI assistant's answer to a question about its past chat with a user, against a gold answer taken from the chat.

Score:
- 1: the answer gives the gold's key fact(s), and nothing it states contradicts the gold. Extra correct detail is fine. Wording may differ.
- 0.5: it gives some of the key facts but misses or garbles others, or it hedges between the right answer and a wrong one.
- 0: it gives a wrong answer, says it cannot find it, or asks for more context, when the gold has an answer.

When the gold says the topic was never discussed: 1 if the answer says it finds no record (it may mention related things it did find); 0 if it states an answer as if it were from the chat.

When the question rests on a false premise (TYPE premise): 1 if the answer rejects or corrects the premise and gives what the gold says was actually so; 0.5 if it doubts the premise but does not give the facts; 0 if it goes along with the premise.

When a fact changed over time (TYPE update): the gold is the latest value and STALE the earlier one. 1 for the latest value (mentioning the earlier one as history is fine); 0 if it gives the stale value as the current one.

Output only JSON: {"score": 0|0.5|1, "why": "one short sentence"}"""


def grade(r):
    if r.get("err") and not r.get("answer"):
        return {**r, "score": 0, "why": "error: " + r["err"]}
    prompt = f"TYPE: {r.get('type', 'fact')}\n\nQUESTION: {r['q']}\n\nGOLD: {r['gold']}\n\n"
    if r.get("stale"):
        prompt += f"STALE: {r['stale']}\n\n"
    prompt += f"ANSWER: {r['answer']}"
    out = subprocess.run(
        ["claude", "-p", "--model", "opus", "--effort", "medium", "--system-prompt", SYSTEM,
         "--tools", "", "--setting-sources", "", "--strict-mcp-config", "--disable-slash-commands",
         "--no-session-persistence", "--output-format", "json"],
        input=prompt, capture_output=True, text=True, timeout=600,
        env={**os.environ, "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"}, cwd="/tmp")
    try:
        res = json.loads(out.stdout)
        if isinstance(res, list):
            res = [e for e in res if e.get("type") == "result"][-1]
        t = res["result"]
        g = json.loads(t[t.index("{"): t.rindex("}") + 1])
    except Exception as e:
        g = {"score": None, "why": "judge failed: " + str(e)[:100]}
    return {**r, **g}


def summary(rows, name):
    def mean(xs):
        xs = [x for x in xs if x is not None]
        return sum(xs) / len(xs) if xs else 0
    facts = [r for r in rows if r["type"] != "abstain"]
    hit = [r for r in facts if set(r.get("evidence") or [r["msg"]]) & set(r.get("recalled") or [])]
    types = {}
    for r in rows:
        types.setdefault(r["type"], []).append(r["score"])
    print(f"{name}: all {mean([r['score'] for r in rows]):.3f} (n={len(rows)}), "
          + ", ".join(f"{t} {mean(s):.2f}/{len(s)}" for t, s in sorted(types.items())) + ", "
          f"evidence recalled {len(hit)}/{len(facts)}, "
          f"tools/q {mean([len(r.get('tools') or []) for r in rows]):.2f}, "
          f"$/q {mean([r.get('cost', 0) for r in rows]):.3f}, "
          f"s/q {mean([r.get('secs', 0) for r in rows]):.0f}")


if __name__ == "__main__":
    for path in sys.argv[1:]:
        rows = [json.loads(l) for l in open(path)]
        out = path.replace(".jsonl", ".graded.jsonl")
        if os.path.exists(out):
            graded = [json.loads(l) for l in open(out)]
        else:
            with ThreadPoolExecutor(8) as ex:
                graded = list(ex.map(grade, rows))
            with open(out, "w") as f:
                for g in graded:
                    f.write(json.dumps(g, ensure_ascii=False) + "\n")
        summary(graded, path)
