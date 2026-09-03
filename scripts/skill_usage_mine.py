#!/usr/bin/env python3
"""Mine skill usage across every agent on this machine and import it into cfrproxy.

Sources:
  hermes       skill_view tool calls in ~/.hermes/profiles/*/sessions/*.jsonl (each call is
               logged twice — as the assistant tool call and again in the tool result — so
               counts are halved)
  claude       Claude Code `Skill` tool invocations plus SKILL.md reads, from the aise index
  codex        SKILL.md reads (shell/exec/apply_patch) from the aise index
  prime-agent  SKILL.md reads from the aise index

Writes a JSON file in the shape `cfrproxy skills import-usage` accepts and, unless --no-import,
runs the import. Re-runs replace each source's counts, so it is safe on a timer:

  python3 scripts/skill_usage_mine.py            # mine + import
  python3 scripts/skill_usage_mine.py --out u.json --no-import
"""
import argparse, collections, glob, json, os, re, subprocess, sys

HOME = os.path.expanduser("~")


def hermes():
    calls, sessions = collections.Counter(), collections.defaultdict(set)

    def walk(obj, sid):
        if isinstance(obj, dict):
            fn = obj.get("function") if isinstance(obj.get("function"), dict) else None
            if ((fn or {}).get("name") or obj.get("name")) == "skill_view":
                args = (fn or obj).get("arguments") or obj.get("input") or obj.get("args")
                if isinstance(args, str):
                    try:
                        args = json.loads(args)
                    except Exception:
                        args = {"name": args}
                if isinstance(args, dict):
                    sk = (args.get("name") or args.get("skill") or "").strip().strip("/")
                    if sk:
                        sk = sk.split("/")[-1]
                        calls[sk] += 1
                        sessions[sk].add(sid)
            for v in obj.values():
                walk(v, sid)
        elif isinstance(obj, list):
            for v in obj:
                walk(v, sid)

    files = glob.glob(f"{HOME}/.hermes/profiles/*/sessions/*.jsonl") + glob.glob(f"{HOME}/.hermes/sessions/*.jsonl")
    for f in files:
        sid = os.path.basename(f)
        try:
            with open(f, errors="ignore") as fh:
                for line in fh:
                    if "skill_view" not in line:
                        continue
                    try:
                        walk(json.loads(line), sid)
                    except Exception:
                        pass
        except Exception:
            pass
    return {k: {"calls": max(1, v // 2), "sessions": len(sessions[k])} for k, v in calls.items()}


def aise_query(sql):
    try:
        out = subprocess.run(["aise", "db", "query", "--format", "json", "--limit", "0", sql],
                             capture_output=True, text=True, timeout=600)
        return json.loads(out.stdout) if out.returncode == 0 and out.stdout.strip() else []
    except Exception as e:
        print("aise unavailable:", e, file=sys.stderr)
        return []


def aise():
    per = collections.defaultdict(lambda: collections.defaultdict(lambda: {"calls": 0, "sessions": set()}))
    for r in aise_query("select provider, session_id, json_extract(content,'$.args.skill') as skill from messages where kind='tool_call' and tool_name='Skill'"):
        sk = (r.get("skill") or "").split(":")[0]
        if sk:
            e = per[r["provider"]][sk]
            e["calls"] += 1
            e["sessions"].add(r["session_id"])
    pat = re.compile(r"([A-Za-z0-9_.\-]+)/SKILL\.md")
    for r in aise_query("select provider, session_id, content from messages where kind='tool_call' and content like '%SKILL.md%'"):
        for n in set(m.group(1) for m in pat.finditer(r["content"])):
            if len(n) < 3 or n in ("skills", "templates", "template"):
                continue
            e = per[r["provider"]][n]
            e["calls"] += 1
            e["sessions"].add(r["session_id"])
    return {src: {n: {"calls": e["calls"], "sessions": len(e["sessions"])} for n, e in d.items()} for src, d in per.items()}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=os.path.expanduser("~/.cfrproxy/skill-usage.json"))
    ap.add_argument("--no-import", action="store_true")
    ap.add_argument("--cfrproxy", default=os.path.expanduser("~/.local/bin/cfrproxy"))
    a = ap.parse_args()
    payload = [{"source": "hermes", "entries": hermes()}]
    for src, entries in aise().items():
        payload.append({"source": src, "entries": entries})
    os.makedirs(os.path.dirname(a.out), exist_ok=True)
    json.dump(payload, open(a.out, "w"))
    for p in payload:
        print(f"{p['source']:12s} {len(p['entries']):4d} skills, {sum(e['calls'] for e in p['entries'].values())} calls")
    if not a.no_import:
        subprocess.run([a.cfrproxy, "skills", "import-usage", a.out], check=False)


if __name__ == "__main__":
    main()
