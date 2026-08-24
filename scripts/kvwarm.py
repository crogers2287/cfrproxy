#!/usr/bin/env python3
"""
kvwarm — prefix-cache warmup and storage manager for the local inference server.

WHY THIS EXISTS
    Measured on fred (cfrproxy traces, 2026-08-19): of 101 requests over 2000
    prompt tokens, the 11 that missed the prefix cache accounted for 395,094 of
    587,585 prefill tokens — 67% of all prefill work — at an average TTFB of
    9,590 ms versus 1,278 ms for a warm request. Established sessions already
    reuse their prefix; it is the FIRST request of a session, and the first
    after a model reload, that pays full price.

    cfrproxy records the static head of every local-model request (system prompt
    + tool schemas) as a content-addressed manifest under the prefix-cache
    namespace. This daemon replays those manifests with max_tokens=1 so the
    static prefix is already resident in the server's KV cache before a real
    request arrives, and prunes the namespace so it does not grow without bound.

CACHE IDENTITY
    A KV prefix is only valid for the exact weights + tokenizer + chat template
    that produced it. Before warming a model, the L0 identity is read from the
    running server (llama-swap /upstream/<model>/props) and hashed:
        model_path + model_ftype + chat_template + bos_token + eos_token
    If that hash changes — a re-quantised model, an edited template — every
    manifest for the model is invalidated rather than replayed against weights
    that would produce different KV.

WHAT IT DOES NOT DO
    It does not warm conversation history (L3). That lives in the inference
    server's own slot and RAM prompt cache and cannot be replayed from outside
    without resending the whole conversation, which would cost more than it saves.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path

DEFAULT_CACHE = Path.home() / ".cfrproxy" / "cache"
DEFAULT_PROXY = "http://127.0.0.1:8420"
DEFAULT_SWAP = "http://127.0.0.1:9069"
USER_AGENT = "cfrproxy-kvwarm/1"
IDENTITY_FILE = "_identity.json"


# ---------------------------------------------------------------- config

@dataclass
class Config:
    cache_root: Path = DEFAULT_CACHE
    proxy: str = DEFAULT_PROXY
    swap: str = DEFAULT_SWAP
    interval: int = 600          # seconds between passes
    max_warm: int = 4            # how many prefixes to keep hot (VRAM bound)
    max_warm_tokens: int = 60000 # total estimated prompt tokens to hold resident
    max_age_days: float = 14.0   # prune manifests unseen for longer than this
    max_entries: int = 200       # prune least-recently-seen beyond this count
    max_bytes: int = 256 << 20   # prune when the namespace exceeds this
    timeout: int = 900
    only_provider: str = ""      # restrict to one provider (e.g. "fred")

    @classmethod
    def load(cls, path: Path | None) -> "Config":
        c = cls()
        if path and path.exists():
            data = json.loads(path.read_text())
            for k, v in data.items():
                if hasattr(c, k):
                    setattr(c, k, Path(v) if k == "cache_root" else v)
        return c


def log(msg: str) -> None:
    print(f"{time.strftime('%Y-%m-%dT%H:%M:%S')} kvwarm: {msg}", flush=True)


# ---------------------------------------------------------------- manifests

@dataclass
class Manifest:
    path: Path
    data: dict
    mtime: float

    @property
    def size(self) -> int:
        return self.path.stat().st_size

    @property
    def est_tokens(self) -> int:
        # Prefer what the server actually reported for this prefix; fall back to
        # a chars/3.5 estimate. Only used for budgeting, never for correctness.
        n = int(self.data.get("last_prompt_tokens") or 0)
        if n:
            return n
        chars = int(self.data.get("system_bytes") or 0) + len(self.data.get("tools") or "")
        return int(chars / 3.5)


def load_manifests(root: Path, only_provider: str = "") -> list[Manifest]:
    out: list[Manifest] = []
    if not root.exists():
        return out
    for p in root.rglob("*.json"):
        if p.name == IDENTITY_FILE:
            continue
        try:
            data = json.loads(p.read_text())
            if data.get("schema") != 1:
                continue
            if only_provider and data.get("provider") != only_provider:
                continue
            out.append(Manifest(p, data, p.stat().st_mtime))
        except (OSError, json.JSONDecodeError):
            continue
    out.sort(key=lambda m: m.mtime, reverse=True)  # most recently seen first
    return out


# ---------------------------------------------------------------- identity

def model_identity(swap: str, model: str, timeout: int = 30) -> tuple[str, dict] | tuple[None, None]:
    """L0 cache identity, read from the running server — never assumed."""
    url = f"{swap.rstrip('/')}/upstream/{model}/props"
    try:
        req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
        with urllib.request.urlopen(req, timeout=timeout) as r:
            props = json.loads(r.read())
    except (urllib.error.URLError, OSError, json.JSONDecodeError) as e:
        log(f"  identity probe failed for {model}: {e}")
        return None, None
    material = "\x00".join([
        str(props.get("model_path", "")),
        str(props.get("model_ftype", "")),
        str(props.get("chat_template", "")),
        str(props.get("bos_token", "")),
        str(props.get("eos_token", "")),
    ])
    ident = {
        "model": model,
        "model_path": props.get("model_path"),
        "model_ftype": props.get("model_ftype"),
        "chat_template_sha256": hashlib.sha256(
            str(props.get("chat_template", "")).encode()).hexdigest(),
        "bos_token": props.get("bos_token"),
        "eos_token": props.get("eos_token"),
        "total_slots": props.get("total_slots"),
        "build_info": props.get("build_info"),
        "l0_fingerprint": hashlib.sha256(material.encode()).hexdigest(),
        "checked": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    return ident["l0_fingerprint"], ident


def check_identity(cfg: Config, model_dir: Path, model: str) -> bool:
    """Return True if the model's weights/template still match what we warmed.

    On a mismatch the stored manifests are stale: their KV would not be
    reusable, so they are removed rather than replayed.
    """
    fp, ident = model_identity(cfg.swap, model, timeout=60)
    if fp is None:
        return False
    idfile = model_dir / IDENTITY_FILE
    prev = None
    if idfile.exists():
        try:
            prev = json.loads(idfile.read_text()).get("l0_fingerprint")
        except (OSError, json.JSONDecodeError):
            prev = None
    idfile.write_text(json.dumps(ident, indent=1))
    if prev and prev != fp:
        removed = 0
        for p in model_dir.rglob("*.json"):
            if p.name != IDENTITY_FILE:
                p.unlink(missing_ok=True)
                removed += 1
        log(f"  {model}: L0 identity changed ({prev[:12]} -> {fp[:12]}), "
            f"invalidated {removed} manifest(s)")
        return False
    return True


# ---------------------------------------------------------------- warming

def warm(cfg: Config, m: Manifest) -> dict | None:
    """Replay a manifest's static prefix with a 1-token generation."""
    d = m.data
    body = {
        "model": f"{d['provider']}/{d['model']}",
        "messages": [
            {"role": "system", "content": d["system"]},
            # A single-character turn: enough to make the request valid without
            # meaningfully extending the prefix we are trying to establish.
            {"role": "user", "content": "."},
        ],
        "max_tokens": 1,
        "temperature": 0,
        "stream": False,
    }
    tools = d.get("tools")
    if tools:
        parsed = json.loads(tools) if isinstance(tools, str) else tools
        if parsed:
            body["tools"] = [
                {"type": "function", "function": {
                    "name": t["name"],
                    "description": t.get("description", ""),
                    "parameters": t.get("parameters", {"type": "object"}),
                }} for t in parsed
            ]
    req = urllib.request.Request(
        f"{cfg.proxy.rstrip('/')}/v1/chat/completions",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "User-Agent": USER_AGENT},
    )
    try:
        with urllib.request.urlopen(req, timeout=cfg.timeout) as r:
            return json.loads(r.read())
    except (urllib.error.URLError, OSError, json.JSONDecodeError) as e:
        log(f"  warm failed for {m.path.name}: {e}")
        return None


CACHE_LOG = Path.home() / ".cfrproxy" / "cache-observability.jsonl"


def report(_resp: dict) -> str:
    """Read the cache telemetry cfrproxy recorded for our own last request.

    The upstream `timings` object does NOT survive back to the client: on the
    translated path cfrproxy rebuilds the response in the inbound dialect and
    drops it. Reading it off the response would silently report every warm as a
    100% hit (reprocessed defaults to 0). cfrproxy captures timings server-side
    into cache-observability.jsonl, so that is the only honest source.
    """
    try:
        with CACHE_LOG.open("rb") as f:
            f.seek(0, os.SEEK_END)
            back = min(f.tell(), 64 * 1024)
            f.seek(-back, os.SEEK_END)
            lines = f.read().decode("utf-8", "replace").splitlines()
    except OSError:
        return "(no telemetry: cache-observability.jsonl unreadable)"
    for line in reversed(lines):
        try:
            d = json.loads(line)
        except json.JSONDecodeError:
            continue
        if d.get("client") != "kvwarm":
            continue
        return (f"ptok={d.get('input_tokens')} reprocessed={d.get('new_prefill_tokens')} "
                f"hit={d.get('prefix_hit_rate_pct', 0):.1f}% "
                f"src={d.get('cache_source')} reason={d.get('cache_reason')} "
                f"pp={d.get('prefill_ms', 0):.0f}ms")
    return "(no telemetry recorded for this warm)"


# ---------------------------------------------------------------- pruning

def prune(cfg: Config, mans: list[Manifest]) -> int:
    """Age / count / size based LRU cleanup. Returns number removed."""
    now = time.time()
    doomed: list[Manifest] = []
    keep: list[Manifest] = []

    for m in mans:
        if (now - m.mtime) > cfg.max_age_days * 86400:
            doomed.append(m)
        else:
            keep.append(m)

    if len(keep) > cfg.max_entries:
        doomed.extend(keep[cfg.max_entries:])
        keep = keep[:cfg.max_entries]

    total = 0
    cut = len(keep)
    for i, m in enumerate(keep):
        try:
            total += m.size
        except OSError:
            continue
        if total > cfg.max_bytes:
            cut = i
            break
    if cut < len(keep):
        doomed.extend(keep[cut:])

    for m in doomed:
        m.path.unlink(missing_ok=True)
    return len(doomed)


# ---------------------------------------------------------------- pass

def one_pass(cfg: Config, dry_run: bool = False) -> None:
    mans = load_manifests(cfg.cache_root, cfg.only_provider)
    if not mans:
        log(f"no manifests under {cfg.cache_root} (nothing has hit a local model yet)")
        return

    removed = 0 if dry_run else prune(cfg, mans)
    if removed:
        log(f"pruned {removed} stale manifest(s)")
        mans = load_manifests(cfg.cache_root, cfg.only_provider)

    # Verify L0 identity once per model directory before warming anything.
    ok_models: dict[str, bool] = {}
    for m in mans:
        mdir = m.path.parent.parent.parent  # <root>/<model-key>/<client>/<scope>/x.json
        key = str(mdir)
        if key not in ok_models:
            ok_models[key] = True if dry_run else check_identity(cfg, mdir, m.data["model"])
    mans = [m for m in mans if ok_models.get(str(m.path.parent.parent.parent), False)]

    # Bounded selection: most-recently-seen first, capped by count and by an
    # estimated token budget so warming can never exhaust the KV pool.
    selected: list[Manifest] = []
    budget = 0
    for m in mans:
        if len(selected) >= cfg.max_warm:
            break
        if budget + m.est_tokens > cfg.max_warm_tokens:
            continue
        selected.append(m)
        budget += m.est_tokens

    log(f"{len(mans)} manifest(s) eligible; warming {len(selected)} "
        f"(~{budget} tokens, cap {cfg.max_warm_tokens})")
    for m in selected:
        d = m.data
        label = f"{d['provider']}/{d['model']} {d['client']}/{d['scope']} {m.path.stem[:12]}"
        if dry_run:
            log(f"  [dry-run] would warm {label} (~{m.est_tokens} tok)")
            continue
        t0 = time.time()
        resp = warm(cfg, m)
        if resp:
            log(f"  warmed {label} in {time.time()-t0:.1f}s :: {report(resp)}")


def main() -> int:
    ap = argparse.ArgumentParser(description="prefix-cache warmup + storage manager")
    ap.add_argument("--config", type=Path, default=Path.home() / ".cfrproxy" / "kvwarm.json")
    ap.add_argument("--cache-root", type=Path)
    ap.add_argument("--proxy"); ap.add_argument("--swap")
    ap.add_argument("--provider", default=None, help="restrict to one provider, e.g. fred")
    ap.add_argument("--max-warm", type=int); ap.add_argument("--max-warm-tokens", type=int)
    ap.add_argument("--interval", type=int)
    ap.add_argument("--once", action="store_true", help="single pass, then exit")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--list", action="store_true", help="show the namespace and exit")
    a = ap.parse_args()

    cfg = Config.load(a.config)
    for src, dst in (("cache_root", "cache_root"), ("proxy", "proxy"), ("swap", "swap"),
                     ("max_warm", "max_warm"), ("max_warm_tokens", "max_warm_tokens"),
                     ("interval", "interval")):
        v = getattr(a, src, None)
        if v is not None:
            setattr(cfg, dst, v)
    if a.provider is not None:
        cfg.only_provider = a.provider

    if a.list:
        mans = load_manifests(cfg.cache_root, cfg.only_provider)
        total = 0
        for m in mans:
            d = m.data
            sz = m.size
            total += sz
            age = (time.time() - m.mtime) / 3600
            print(f"{d['provider']:>8}/{d['model']:<12} {d['client']:<12} {d['scope']:<9} "
                  f"tools={d.get('tool_count',0):<3} ~{m.est_tokens:>7} tok  "
                  f"{sz/1024:>7.1f} KiB  {age:>6.1f}h ago  {m.path.stem[:12]}")
        print(f"\n{len(mans)} manifest(s), {total/1048576:.2f} MiB under {cfg.cache_root}")
        return 0

    if a.once or a.dry_run:
        one_pass(cfg, dry_run=a.dry_run)
        return 0

    log(f"starting: cache={cfg.cache_root} proxy={cfg.proxy} interval={cfg.interval}s "
        f"max_warm={cfg.max_warm} budget={cfg.max_warm_tokens} tok")
    while True:
        try:
            one_pass(cfg)
        except Exception as e:  # a warmup daemon must never take itself down
            log(f"pass failed: {e!r}")
        time.sleep(cfg.interval)


if __name__ == "__main__":
    sys.exit(main())
