"""Draw questions from a copy of a chat: one per sampled message, written by
opus, with the message's id and a gold answer.

    uv run --no-project python eval/gen.py <chat dir> <questions.jsonl> [n]

The questions hold the chat's facts: keep them out of the repo. Lines added
by hand with "msg": -1 and "type": "abstain" ask about things the chat never
held (check with grep); judge.py expects a "no record" answer to those.
"""
import glob, json, os, random, subprocess, sys
from concurrent.futures import ThreadPoolExecutor

CHAT = sys.argv[1]
OUT = sys.argv[2]
N = int(sys.argv[3]) if len(sys.argv) > 3 else 60
random.seed(int(os.environ.get("SEED", "17")))

msgs = []
for f in sorted(glob.glob(f"{CHAT}/main/*.jsonl")):
    for l in open(f):
        msgs.append(json.loads(l))

def ok(m, lo, hi):
    t = m["text"]
    if not (lo <= len(t) <= hi):
        return False
    w = t.split(" ", 1)[0]
    return not (m["kind"] == "tool" and w in ("zoom", "search", "date"))

pools = {
    "opt": [m for m in msgs if m["kind"] == "opt" and ok(m, 300, 4000)],
    "user": [m for m in msgs if m["kind"] == "user" and ok(m, 150, 2000)],
    "work": [m for m in msgs if m["kind"] == "work" and ok(m, 300, 20000)],
    "echo": [m for m in msgs if m["kind"] == "echo" and ok(m, 200, 4000)],
    "note": [m for m in msgs if m["kind"] == "note" and ok(m, 150, 3000)],
}
share = {"opt": 0.4, "user": 0.3, "work": 0.1, "echo": 0.1, "note": 0.1}
picked = []
for k, s in share.items():
    picked += random.sample(pools[k], min(len(pools[k]), round(N * s)))

SYSTEM = """You write test questions for a memory system. A user has one endless chat with an AI assistant called Opt. Later, the user will ask Opt about something from the past, and Opt must find it in its memory of the chat.

You get one TARGET message from that chat (with its kind, id and date) and the two messages before it for context.

Write ONE question the user could naturally ask Opt weeks later, whose answer is a specific fact stated in the TARGET message: a name, number, path, setting, decision, reason, error, result, or date.

Rules:
- The question must make sense on its own, with no access to the target: name the project, tool or topic so that only one answer fits across months of chat.
- Phrase it as the user would, in the user's language and register. Names and terms are fine; do not copy long phrases from the target.
- The answer must be in the TARGET message itself, not only in the context messages.
- Prefer facts with lasting value (decisions, settings, causes, names, numbers) over trivia.
- If the target holds no such fact (chit-chat, a bare acknowledgement, a fragment), output {"ok": false}.

Output only JSON: {"ok": true, "q": "...", "gold": "the short answer, with the key fact(s)", "type": "fact|decision|reason|date"}"""

def ask(m):
    i = m["i"]
    ctx = [x for x in msgs[max(0, i - 2):i]]
    def fmt(x, n):
        return f"[{x['kind']} {x['i']}, {x['date'][:16]}]\n{x['text'][:n]}"
    prompt = "CONTEXT:\n" + "\n\n".join(fmt(x, 1500) for x in ctx) + "\n\nTARGET:\n" + fmt(m, 6000)
    r = subprocess.run(
        ["claude", "-p", "--model", "opus", "--effort", "medium", "--system-prompt", SYSTEM,
         "--tools", "", "--setting-sources", "", "--strict-mcp-config", "--disable-slash-commands",
         "--no-session-persistence", "--output-format", "json"],
        input=prompt, capture_output=True, text=True, timeout=600,
        env={**os.environ, "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"}, cwd="/tmp")
    try:
        res = json.loads(r.stdout)
        if isinstance(res, list):
            res = [e for e in res if e.get("type") == "result"][-1]
        t = res["result"].strip()
        t = t[t.index("{"): t.rindex("}") + 1]
        q = json.loads(t)
        q["cost"] = res.get("total_cost_usd", 0)
    except Exception as e:
        return {"ok": False, "err": str(e)[:200], "msg": i}
    q.update(msg=i, kind=m["kind"], date=m["date"][:10])
    return q

with ThreadPoolExecutor(8) as ex:
    qs = list(ex.map(ask, picked))
good = [q for q in qs if q.get("ok")]
with open(OUT, "w") as f:
    for n, q in enumerate(good):
        q["id"] = f"q{n+1}"
        f.write(json.dumps(q, ensure_ascii=False) + "\n")
print(f"{len(good)} of {len(qs)} kept; ${sum(q.get('cost', 0) for q in qs):.2f}")
