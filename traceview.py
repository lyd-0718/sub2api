import sys, json, gzip, glob, os

d = sys.argv[1]
files = sorted(glob.glob(os.path.join(d, "*.json.gz")))
turn = int(sys.argv[2]) if len(sys.argv) > 2 else None
show = sys.argv[3] if len(sys.argv) > 3 else "all"

def clip(s, n=500):
    if not isinstance(s, str):
        s = json.dumps(s, ensure_ascii=False)
    return s if len(s) <= n else s[:n] + " ...[truncated, total %d chars]" % len(s)

for i, f in enumerate(files, 1):
    if turn and i != turn:
        continue
    rec = json.load(gzip.open(f))
    req, r = rec["request"], rec["response"]
    t = rec["started_at"][11:19]
    print()
    print("=" * 70)
    print("turn %d  %s  %s  http=%s  complete=%s" % (i, t, rec["model"], rec["http_status"], r["complete"]))
    if show in ("all", "user"):
        msgs = req.get("messages", [])
        if msgs:
            last = msgs[-1]
            c = last.get("content")
            if isinstance(c, list):
                for part in c:
                    pt = part.get("type")
                    if pt == "text":
                        print("\n[USER INPUT]\n" + clip(part.get("text", "")))
                    elif pt == "tool_result":
                        print("\n[TOOL RESULT]\n" + clip(str(part.get("content", "")), 300))
            else:
                print("\n[USER INPUT]\n" + clip(str(c)))
    for b in r.get("blocks", []):
        bt = b["type"]
        if bt == "thinking" and show in ("all", "think"):
            print("\n[THINKING]\n" + clip(b.get("thinking", "")))
        elif bt == "tool_use" and show in ("all", "tool"):
            print("\n[TOOL CALL] " + b["tool_name"] + "\n" + clip(b.get("tool_input"), 300))
        elif bt == "text" and show in ("all", "text"):
            print("\n[ANSWER]\n" + clip(b.get("text", "")))
