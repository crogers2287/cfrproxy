#!/usr/bin/env python3
"""Sync Hermes's Telegram /model picker to cfrproxy's live provider set.

Gives every Hermes agent a router->provider->model drill-down:
  /model -> "cfrproxy" group -> pick a provider (e.g. openrouter/ollama/anthropic)
         -> pick a model (that provider's live catalog)

Two coordinated pieces (see cfrproxy/HERMES_INTEGRATION.md):
  1. One Hermes custom_providers entry per enabled cfrproxy provider, each
     pointing at that provider's scoped mount http://HOST/p/<name>/v1 so it
     lists only that provider's models (bare ids) and routes only to it.
  2. A "cfrproxy" entry in hermes_cli.models.PROVIDER_GROUPS listing those
     providers' slugs — the only mechanism that produces the group tier.

Re-run after adding/removing a provider in cfrproxy. Idempotent. Models
*within* a provider are always live (Hermes probes /v1/models); only
provider add/remove needs a re-run.

Env overrides: CFRPROXY_ADMIN (default http://127.0.0.1:8420),
CFRPROXY_ADMIN_USER/PASS, HERMES_HOME_ROOT (default ~/.hermes),
HERMES_AGENT (default ~/.hermes/hermes-agent).
"""
import base64
import glob
import json
import os
import shutil
import sys
import time
import urllib.request

from ruamel.yaml import YAML

HOME = os.path.expanduser("~")
ADMIN = os.environ.get("CFRPROXY_ADMIN", "http://127.0.0.1:8420").rstrip("/")
ADMIN_USER = os.environ.get("CFRPROXY_ADMIN_USER", "admin")
ADMIN_PASS = os.environ.get("CFRPROXY_ADMIN_PASS", "")
# data-plane host the gateways call (same box)
DATA_HOST = os.environ.get("CFRPROXY_HOST", "http://127.0.0.1:8420").rstrip("/")
HERMES_ROOT = os.environ.get("HERMES_HOME_ROOT", os.path.join(HOME, ".hermes"))
HERMES_AGENT = os.environ.get("HERMES_AGENT", os.path.join(HERMES_ROOT, "hermes-agent"))
MODELS_PY = os.path.join(HERMES_AGENT, "hermes_cli", "models.py")

GROUP_ID = "cfrproxy"
GROUP_LABEL = "cfrproxy"
GROUP_DESC = "Local LLM router — pick a provider, then a model"
BEGIN = "# --- cfrproxy dynamic group (managed by sync_hermes_cfrproxy.py) ---"
END = "# --- end cfrproxy dynamic group ---"


def fetch_providers():
    req = urllib.request.Request(ADMIN + "/admin/api/providers")
    if ADMIN_PASS:
        tok = base64.b64encode(f"{ADMIN_USER}:{ADMIN_PASS}".encode()).decode()
        req.add_header("Authorization", "Basic " + tok)
    with urllib.request.urlopen(req, timeout=10) as r:
        data = json.load(r)
    return [p for p in data if p.get("enabled")]


def hermes_entry(name):
    return {
        "name": f"cfrproxy-{name}",
        "base_url": f"{DATA_HOST}/p/{name}/v1",
        "api_key": "cfrproxy",
        "api_mode": "chat_completions",
        "discover_models": True,
    }


def sync_profile(cfg_path, providers):
    yaml = YAML()
    yaml.preserve_quotes = True
    with open(cfg_path) as f:
        doc = yaml.load(f)
    if doc is None:
        return "empty, skipped"
    cps = doc.get("custom_providers")
    if cps is None:
        cps = []
        doc["custom_providers"] = cps
    # drop any prior cfrproxy-managed entries (single 'cfrproxy' or 'cfrproxy-*')
    kept = [e for e in cps
            if not str((e or {}).get("name", "")).lower().startswith("cfrproxy")]
    new_entries = [hermes_entry(p["name"]) for p in providers]
    doc["custom_providers"] = new_entries + kept
    shutil.copy(cfg_path, cfg_path + f".bak-cfrsync-{int(time.time())}")
    with open(cfg_path, "w") as f:
        yaml.dump(doc, f)
    return f"{len(new_entries)} cfrproxy providers, {len(kept)} others kept"


def patch_provider_groups(providers):
    """Insert/replace a managed block that adds the cfrproxy group to
    PROVIDER_GROUPS, placed between the dict literal and _SLUG_TO_GROUP so the
    reverse index picks it up automatically."""
    src = open(MODELS_PY).read()
    slugs = [f"custom:cfrproxy-{p['name'].strip().lower()}" for p in providers]
    block = (
        f"{BEGIN}\n"
        f"PROVIDER_GROUPS[{GROUP_ID!r}] = (\n"
        f"    {GROUP_LABEL!r},\n"
        f"    {GROUP_DESC!r},\n"
        f"    {slugs!r},\n"
        f")\n"
        f"{END}\n"
    )
    if BEGIN in src and END in src:
        pre = src[: src.index(BEGIN)]
        post = src[src.index(END) + len(END) + 1:]
        new = pre + block + post
    else:
        anchor = "_SLUG_TO_GROUP: dict[str, str] = {"
        if anchor not in src:
            raise SystemExit("could not find _SLUG_TO_GROUP anchor in models.py")
        i = src.index(anchor)
        new = src[:i] + block + "\n" + src[i:]
    if new != src:
        shutil.copy(MODELS_PY, MODELS_PY + f".bak-cfrsync-{int(time.time())}")
        open(MODELS_PY, "w").write(new)
    return slugs


def clear_caches():
    for pat in ("picker_output_cache.json", "provider_models_cache.json"):
        for f in glob.glob(os.path.join(HERMES_ROOT, "**", pat), recursive=True):
            try:
                os.remove(f)
            except OSError:
                pass


def main():
    providers = fetch_providers()
    if not providers:
        raise SystemExit("no enabled providers from cfrproxy admin API")
    names = [p["name"] for p in providers]
    print(f"cfrproxy enabled providers: {names}")

    profiles = sorted(glob.glob(os.path.join(HERMES_ROOT, "profiles", "*", "config.yaml")))
    if not profiles:
        # single-config install
        single = os.path.join(HERMES_ROOT, "config.yaml")
        if os.path.exists(single):
            profiles = [single]
    for cfg in profiles:
        prof = os.path.basename(os.path.dirname(cfg))
        print(f"  {prof}: {sync_profile(cfg, providers)}")

    slugs = patch_provider_groups(providers)
    print(f"PROVIDER_GROUPS['cfrproxy'] members: {slugs}")
    clear_caches()
    print("picker caches cleared. Restart gateways to load config:")
    print("  systemctl --user restart 'hermes-gateway-*'")


if __name__ == "__main__":
    sys.exit(main())
