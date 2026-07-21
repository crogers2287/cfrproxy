import os
import sys, shutil, time, os

BLOCK = """- name: cfrproxy
  base_url: http://127.0.0.1:8420/v1
  api_key: cfrproxy
  api_mode: chat_completions
  discover_models: true
"""

profiles = ["ash","canna","fogger","grant","haxor","max","winston"]
for p in profiles:
    f = os.path.expanduser(f"~/.hermes/profiles/{p}/config.yaml")
    src = open(f).read()
    if "name: cfrproxy" in src:
        print(f"{p}: already has cfrproxy, skip")
        continue
    if "\ncustom_providers:" not in src and not src.startswith("custom_providers:"):
        print(f"{p}: NO custom_providers key — SKIP (needs manual)")
        continue
    shutil.copy(f, f + f".bak-cfrproxy-{int(time.time())}")
    lines = src.splitlines(keepends=True)
    out = []
    inserted = False
    for line in lines:
        out.append(line)
        if not inserted and line.rstrip() == "custom_providers:":
            # indent block entries to match a top-level list under the key
            out.append(BLOCK)
            inserted = True
    if not inserted:
        print(f"{p}: custom_providers: line not found exactly — SKIP")
        continue
    open(f, "w").write("".join(out))
    print(f"{p}: injected cfrproxy")
