"""Draw harder questions from a copy of a chat than gen.py does: ones that
need several messages, the order of events, a fact the chat later changed,
or a question built on a false premise; ones worded without the target's
own words; and the chat's own lookups, a user message after which Opt
opened an old message, rewritten as the question the user was asking.
After the abilities LongMemEval tests (https://arxiv.org/abs/2410.10813):
multi-session reasoning, temporal reasoning, knowledge updates and
abstention; and MemTrapBench's misleading memories
(https://arxiv.org/html/2608.20202).

    uv run --no-project python eval/gen2.py <chat dir> <questions.jsonl>

Each kind's count comes from N_<KIND> in the environment (paraphrase,
multi, order, update, premise; real takes every lookup it can). opus may
turn a draw down, so expect fewer. The questions hold the chat's facts:
keep them out of the repo.
"""
import glob, json, math, os, random, re, subprocess, sys
from collections import Counter
from concurrent.futures import ThreadPoolExecutor

CHAT, OUT = sys.argv[1], sys.argv[2]
random.seed(int(os.environ.get("SEED", "29")))
N = {k: int(os.environ.get("N_" + k.upper(), d))
     for k, d in (("paraphrase", 24), ("multi", 30), ("order", 24), ("update", 40), ("premise", 24))}

msgs = [json.loads(l) for f in sorted(glob.glob(f"{CHAT}/main/*.jsonl")) for l in open(f)]
word = re.compile(r"[^\W_]{3,}")
toks = [set(word.findall(m["text"].lower())) for m in msgs]
df = Counter(t for ts in toks for t in ts)
idf = {t: math.log(len(msgs) / d) for t, d in df.items()}
avg = sum(len(m["text"]) for m in msgs) / len(msgs)
index = {}
for i, ts in enumerate(toks):
    for t in ts:
        if df[t] <= 300:
            index.setdefault(t, []).append(i)


def lookup(m):
    return m["kind"] == "tool" and m["text"].split(" ", 1)[0] in ("zoom", "search", "date")


def related(i, k=8):
    """The k messages, away from i, that share i's rarest words most."""
    rare = sorted((t for t in toks[i] if 2 <= df[t] <= 300), key=lambda t: -idf[t])[:12]
    score = Counter()
    for t in rare:
        for j in index[t]:
            if abs(j - i) > 5:
                score[j] += idf[t]
    for j in score:
        score[j] *= 2.2 / (1 + 1.2 * (0.7 + 0.3 * len(msgs[j]["text"]) / avg))
    out = [j for j, _ in score.most_common(40) if not lookup(msgs[j]) and len(msgs[j]["text"]) >= 60]
    return out[:k]


def fmt(m, n):
    return f"[{m['kind']} {m['i']}, {m['date'][:16]}]\n{m['text'][:n]}"


def ok(m, lo, hi):
    return lo <= len(m["text"]) <= hi and not lookup(m) and m["kind"] in ("opt", "user", "work", "echo", "note")


COMMON = """A user has one endless chat with an AI assistant called Opt. Later, the user asks Opt about the past, and Opt must answer from its memory of the chat. You write one test question for that memory.

Rules for every question:
- It must make sense on its own, weeks later: name the project, tool or topic so that one answer fits across months of chat.
- Phrase it as the user would, in the user's register and language (mostly English, sometimes Russian).
- The gold answer must follow from the messages you are given. If they do not support a good question of the kind asked, output {"ok": false}.
- Prefer lasting facts (decisions, settings, causes, names, numbers, outcomes) over trivia.

Output only JSON: {"ok": true, "q": "...", "gold": "the short answer with its key facts", "evidence": [ids of the messages that hold the answer]%s}"""

TASKS = {
    "paraphrase": ("""

You get one TARGET message, with the two before it for context. Write a question whose answer is a specific fact in TARGET, worded the way someone recalling it vaguely weeks later would: describe the thing by what it did or what it was about, NOT by the rare words, names, numbers or phrases TARGET uses for it. Ordinary words are fine; any word distinctive of TARGET is not. A reader of only the question should not be able to grep TARGET with it.""", ""),
    "multi": ("""

You get several messages from different times that share a topic. Write a question whose answer needs facts from at least TWO of them, from different days or turns where possible: e.g. a total, a comparison, a list gathered from several places, or what connects two things. One message alone must not be enough.""", ""),
    "order": ("""

You get several messages from different times that share a topic, with their dates. Write a question about time or order: which came first, what was done after something, how long between two events, what the state was on a given day, or when something happened. The gold must give the dates or order the messages show.""", ""),
    "update": ("""

You get several messages from different times that share a topic. Look for a fact that a LATER message changed: a setting, number, decision, name, plan or status that was first one thing and later another (a correction, a reversal, a new value). If there is one, ask what that fact is NOW, without hinting that it changed. The gold is the latest value; "stale" is the earlier value, a wrong answer. If no fact clearly changed, output {"ok": false}.""", ', "stale": "the earlier value"'),
    "premise": ("""

You get several messages that share a topic. Write a question that rests on a FALSE premise close to the truth, the kind a user with a fuzzy memory would ask: it gives a wrong number, credits a decision or a fact to the wrong project or tool, or asks why something was done that was in fact not done (or was done the other way). The messages must show that the premise is false. The gold says what was actually so. A good answer corrects the premise; a bad one goes along with it.""", ""),
}

REAL = COMMON % "" + """

You get a REAL message the user sent, and an OLD message that Opt opened from its memory right after, to deal with it. Write the question about the past that the user's message was really asking, in the user's own words as far as possible (keep their wording, vagueness and language), whose answer is in OLD. If the user's message needs nothing from OLD's facts (Opt opened it only for context), output {"ok": false}."""


def claude(system, prompt):
    r = subprocess.run(
        ["claude", "-p", "--model", "opus", "--effort", "medium", "--system-prompt", system,
         "--tools", "", "--setting-sources", "", "--strict-mcp-config", "--disable-slash-commands",
         "--no-session-persistence", "--output-format", "json"],
        input=prompt, capture_output=True, text=True, timeout=900,
        env={**os.environ, "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"}, cwd="/tmp")
    res = json.loads(r.stdout)
    if isinstance(res, list):
        res = [e for e in res if e.get("type") == "result"][-1]
    t = res["result"].strip()
    q = json.loads(t[t.index("{"): t.rindex("}") + 1])
    q["cost"] = res.get("total_cost_usd", 0)
    return q


def ask(job):
    kind, i = job
    try:
        if kind == "real":
            u, old = i
            q = claude(REAL, "REAL:\n" + fmt(msgs[u], 3000) + "\n\nOLD:\n" + fmt(msgs[old], 6000))
            q.update(msg=old, before=u, evidence=[old])
        elif kind == "paraphrase":
            ctx = msgs[max(0, i - 2):i]
            q = claude(COMMON % "" + TASKS[kind][0],
                       "CONTEXT:\n" + "\n\n".join(fmt(x, 1500) for x in ctx) + "\n\nTARGET:\n" + fmt(msgs[i], 6000))
            q.update(msg=i, evidence=[i])
        else:
            ids = sorted([i] + related(i))
            q = claude(COMMON % TASKS[kind][1] + TASKS[kind][0],
                       "MESSAGES, oldest first:\n\n" + "\n\n".join(fmt(msgs[j], 2500) for j in ids))
            q["msg"] = max(q.get("evidence") or [i])
    except Exception as e:
        return {"ok": False, "err": str(e)[:200], "type": kind}
    q["type"] = kind
    return q


pool = [m["i"] for m in msgs if ok(m, 250, 8000)]
jobs, used = [], set()
for kind, n in N.items():
    for i in random.sample([i for i in pool if i not in used], n):
        used.add(i)
        jobs.append((kind, i))
last = None
for m in msgs:
    if m["kind"] == "user":
        last = m["i"]
    if m["kind"] == "tool" and m["text"].startswith("zoom "):
        try:
            old = int(json.loads(m["text"][5:])["id"])
        except (ValueError, KeyError, TypeError):
            continue
        if last is not None and old < last - 20 and ok(msgs[old], 1, 10**6):
            jobs.append(("real", (last, old)))

with ThreadPoolExecutor(8) as ex:
    qs = list(ex.map(ask, jobs))
good = [q for q in qs if q.get("ok")]
seen = set()
with open(OUT, "w") as f:
    for q in good:
        if q["q"] in seen:
            continue
        seen.add(q["q"])
        q["id"] = f"{q['type']}{len(seen)}"
        f.write(json.dumps(q, ensure_ascii=False) + "\n")
print(f"{len(seen)} of {len(qs)} kept, {Counter(q['type'] for q in good)}; ${sum(q.get('cost', 0) for q in qs):.2f}")
