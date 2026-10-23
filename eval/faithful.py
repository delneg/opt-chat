"""Count the claims in -eval-compact's lines that their input doesn't hold,
with a judge model (sonnet), and print a summary per node and overall.

    uv run --no-project python eval/faithful.py lines.jsonl [more.jsonl ...]
    ... --phrase "some text"   also count the lines that hold this text

A line may name ids, people or the request a step serves from its context:
that resolves a reference. It may not add what its input lacks: lists of
owed or open items, verdicts ("wrong", "should be"), outcomes or states.
See docs/2026-10-23-faithful-lines.md.
"""
import json, subprocess, sys
from collections import defaultdict
from concurrent.futures import ThreadPoolExecutor

SYSTEM = """You check one line of an AI assistant's memory against the input it was written from. The line was written with the rest of the chat as context, which you don't see.

List each claim in the line that the input doesn't support. Count as unsupported:
- a list of owed, open, pending or to-do items, or any item in one, that the input doesn't hold;
- a verdict the input doesn't make: that something is wrong, should be, must be, is fixed or broken;
- an outcome, state or result the input doesn't show;
- a fact about other work that the input doesn't mention.

Don't count: message ids, names, paths or numbers that identify what the input refers to; saying which request a step serves; tags like "tool:" or "echo:"; dropped details.

Output only JSON: {"unsupported": ["short quote of each claim", ...]}"""


def judge(r):
    prompt = f"<input>\n{r['input']}\n</input>\n<line>\n{r['line']}\n</line>"
    for _ in range(3):
        p = subprocess.run(
            ["claude", "-p", "--model", "sonnet", "--tools", "", "--system-prompt", SYSTEM,
             "--settings", '{"disableAllHooks":true}', "--no-session-persistence", prompt],
            capture_output=True, text=True, timeout=300)
        out = p.stdout.strip()
        s, e = out.find("{"), out.rfind("}")
        try:
            return {**r, "unsupported": json.loads(out[s:e + 1])["unsupported"]}
        except Exception:
            continue
    return {**r, "unsupported": None}


def main():
    args, phrase = sys.argv[1:], None
    if "--phrase" in args:
        j = args.index("--phrase")
        phrase = args[j + 1]
        del args[j:j + 2]
    for path in args:
        rows = [json.loads(l) for l in open(path) if l.strip()]
        rows = [r for r in rows if "line" in r and r["line"]]
        with ThreadPoolExecutor(8) as ex:
            graded = list(ex.map(judge, rows))
        with open(path.replace(".jsonl", ".faithful.jsonl"), "w") as f:
            for g in graded:
                f.write(json.dumps(g) + "\n")
        by = defaultdict(list)
        for g in graded:
            by[f"{g['i']}+{1 << g['l']}"].append(g)
        print(path)
        tot = bad = claims = hits = 0
        for k, gs in by.items():
            ok = [g for g in gs if g["unsupported"] is not None]
            b = sum(1 for g in ok if g["unsupported"])
            c = sum(len(g["unsupported"]) for g in ok)
            h = sum(1 for g in gs if phrase and phrase in g["line"])
            tot, bad, claims, hits = tot + len(ok), bad + b, claims + c, hits + h
            extra = f", {h}/{len(gs)} hold the phrase" if phrase else ""
            print(f"  {k}: {b}/{len(ok)} lines add claims, {c} claims{extra}")
        extra = f"; {hits} hold the phrase" if phrase else ""
        print(f"  all: {bad}/{tot} lines add claims ({bad / max(tot, 1):.0%}), {claims} claims{extra}")


main()
