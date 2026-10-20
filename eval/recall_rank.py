"""Rank each question's source message under opt-chat's recall scoring, for
several settings, with no model call. Two sets: the questions gen.py wrote,
and the chat's own lookups (a user message, then Opt's zoom(id, 1) into a
message 20 or more back).

    uv run --no-project python eval/recall_rank.py <chat dir> <questions.jsonl>

The scoring follows Chat.wordHits in chat.go; keep the two in step.
"""
import glob, json, math, re, sys

chat, questions = sys.argv[1], sys.argv[2]
msgs = [json.loads(l) for f in sorted(glob.glob(f"{chat}/main/*.jsonl")) for l in open(f)]
lookups = {m["ref"] for m in msgs
           if m["kind"] == "tool" and m.get("ref") and m["text"].split(" ", 1)[0] in ("zoom", "search", "date")}
nonword = re.compile(r"[\W_]+")
lows = [m["text"].lower() for m in msgs]
lens = [len(m["text"]) for m in msgs]


def scores(q, before, k1=1.2, b=0.3, square=True):
    """[(score, i)], best first, as wordHits ranks them."""
    terms = []
    for w in nonword.split(q.lower()):
        if len(w) >= 3 and w not in terms and len(terms) < 64:
            terms.append(w)
    n = total = 0
    df = [0] * len(terms)
    hits = []
    for i in range(before):
        if msgs[i].get("ref") in lookups:
            continue
        n += 1
        total += lens[i]
        found = [t for t, term in enumerate(terms) if term in lows[i]]
        for t in found:
            df[t] += 1
        if found:
            hits.append((i, found))
    if not hits:
        return []
    weight = [max(0, math.log((n - d + 0.5) / (d + 0.5))) for d in df]
    if square:
        weight = [w * w for w in weight]
    avg = total / n
    sc = sorted(((k1 + 1) / (1 + k1 * (1 - b + b * lens[i] / avg)) * sum(weight[t] for t in f), i) for i, f in hits)
    sc.sort(key=lambda x: (-x[0], -x[1]))
    return [x for x in sc if x[0] > 0 and x[0] >= sc[0][0] / 4]


def rank(q, golds, before, **kw):
    for r, (_, i) in enumerate(scores(q, before, **kw)):
        if i in golds:
            return r + 1
    return 10**6


gen = [json.loads(l) for l in open(questions)]
gen = [(q["q"], {q["msg"]}, len(msgs)) for q in gen if q["msg"] >= 0]
real, last = {}, None
for m in msgs:
    if m["kind"] == "user":
        last = m["i"]
    if m["kind"] == "tool" and m["text"].startswith("zoom "):
        try:
            a = json.loads(m["text"][5:])
            old = int(a["id"])  # the model sends it as a number or a string
        except (ValueError, KeyError, TypeError):
            continue
        # a message opened: in mcp.go, n left out or 0 opens it too
        if a.get("n") in (None, 0, 1) and last is not None and old < last - 20:
            real.setdefault(last, set()).add(old)
real = [(msgs[u]["text"], g, u) for u, g in real.items()]

for name, rows in (("generated", gen), ("lookups", real)):
    if not rows:
        continue
    for square in (True, False):
        for b in (0.75, 0.5, 0.3, 0.0):
            rs = [rank(q, g, u, b=b, square=square) for q, g, u in rows]
            print(f"{name:9} weight{'²' if square else ' '} b={b:<4}: in the first 8 {sum(r <= 8 for r in rs)}/{len(rs)}, "
                  f"MRR {sum(1 / r for r in rs) / len(rs):.3f}")
