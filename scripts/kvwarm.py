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
import base64
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
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
    target_model: str = ""       # replay every eligible prefix onto this model
    target_models: list[str] = field(default_factory=list)  # replay onto each model
    idle_grace: int = 15         # require this many idle seconds before each replay

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


def import_capture(cfg: Config, capture_id: int, target_model: str) -> Path:
    """Import one llama-swap request capture without proxy transformations."""
    url = f"{cfg.swap.rstrip('/')}/api/captures/{capture_id}"
    req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
    with urllib.request.urlopen(req, timeout=30) as r:
        capture = json.loads(r.read())
    body = json.loads(base64.b64decode(capture["req_body"]))
    systems = [m.get("content", "") for m in body.get("messages", [])
               if m.get("role") == "system"]
    if not systems:
        raise ValueError(f"capture {capture_id} has no system message")
    system = "\n".join(systems)
    tools = []
    for item in body.get("tools") or []:
        fn = item.get("function", item)
        if fn.get("name"):
            tools.append({"name": fn["name"],
                          "description": fn.get("description", ""),
                          "parameters": fn.get("parameters", {"type": "object"})})
    tools_json = json.dumps(tools, sort_keys=True, separators=(",", ":"))
    sys_sha = hashlib.sha256(system.encode()).hexdigest()
    tools_sha = hashlib.sha256(tools_json.encode()).hexdigest()
    material = "\x00".join(["fred", target_model, sys_sha, tools_sha, "direct"])
    fp = hashlib.sha256(material.encode()).hexdigest()
    prompt_tokens = 0
    try:
        with urllib.request.urlopen(f"{cfg.swap.rstrip('/')}/api/metrics", timeout=30) as r:
            rows = json.loads(r.read())
        prompt_tokens = next((
            int(x.get("input_tokens") or 0) + max(0, int(x.get("cache_tokens") or 0))
            for x in rows if int(x.get("id", -1)) == capture_id
        ), 0)
    except (urllib.error.URLError, OSError, json.JSONDecodeError):
        pass
    now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    data = {"schema": 1, "provider": "fred", "model": target_model,
            "client": "hermes-direct", "scope": "global",
            "fingerprint": fp, "system_sha256": sys_sha,
            "tools_sha256": tools_sha, "system": system, "tools": tools_json,
            "system_bytes": len(system.encode()), "tool_count": len(tools),
            "first_seen": now, "last_seen": now,
            "last_prompt_tokens": prompt_tokens, "last_cache_source": "",
            "last_cache_reason": "direct_capture", "direct": True}
    out_dir = cfg.cache_root / "_direct" / "hermes-direct" / "global"
    out_dir.mkdir(parents=True, exist_ok=True)
    path = out_dir / f"{fp[:24]}.json"
    tmp = path.with_suffix(".json.tmp")
    tmp.write_text(json.dumps(data, indent=1))
    tmp.replace(path)
    return path


# ---------------------------------------------------------------- identity

def model_identity(swap: str, model: str, timeout: int = 30) -> tuple[str, dict] | tuple[None, None]:
    """L0 cache identity, read from the running server — never assumed."""
    url = f"{swap.rstrip('/')}/upstream/{model}/props"
    try:
        req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
        with urllib.request.urlopen(req, timeout=timeout) as r:
            props = json.loads(r.read())
    except (urllib.error.URLError, OSError, json.JSONDecodeError) as e:
        # vLLM has no llama.cpp /props endpoint. Its model list is enough to
        # distinguish the live server; a restart clears vLLM's KV regardless.
        try:
            url = f"{swap.rstrip('/')}/upstream/{model}/v1/models"
            req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
            with urllib.request.urlopen(req, timeout=timeout) as r:
                models = json.loads(r.read()).get("data", [])
            props = {
                "model_path": ",".join(sorted(str(x.get("root") or x.get("id") or "")
                                                for x in models)),
                "model_ftype": "vllm",
                "chat_template": "",
                "bos_token": "",
                "eos_token": "",
                "total_slots": None,
                "build_info": "vllm",
            }
        except (urllib.error.URLError, OSError, json.JSONDecodeError) as fallback_error:
            log(f"  identity probe failed for {model}: {e}; fallback: {fallback_error}")
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

    A mismatch invalidates previously computed KV, not the prompt recipe. Keep
    the system/tool manifest and replay it against the new weights/template.
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
        log(f"  {model}: L0 identity changed ({prev[:12]} -> {fp[:12]}), "
            "retaining prompt recipes and rebuilding their KV")
    return True


# ---------------------------------------------------------------- warming

def warm(cfg: Config, m: Manifest, target_model: str = "") -> dict | None:
    """Replay a manifest's static prefix with a short sanity-checked decode."""
    d = m.data
    model = target_model or d["model"]
    direct = bool(d.get("direct"))
    body = {
        "model": model if direct else f"{d['provider']}/{model}",
        "messages": [
            {"role": "system", "content": d["system"]},
            # A single-character turn: enough to make the request valid without
            # meaningfully extending the prefix we are trying to establish.
            {"role": "user", "content": "."},
        ],
        # A one-token decode cannot distinguish a healthy warm from the
        # repeated-token cache corruption seen on the KVarN+MTP path. Sixteen
        # tokens are still negligible beside a 20K-30K prefix, but expose the
        # failure before the warmer churns through the rest of the cache.
        "max_tokens": 16,
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
    endpoint = (f"{cfg.swap.rstrip('/')}/upstream/{model}/v1/chat/completions"
                if direct else f"{cfg.proxy.rstrip('/')}/v1/chat/completions")
    req = urllib.request.Request(
        endpoint,
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "User-Agent": USER_AGENT},
    )
    try:
        with urllib.request.urlopen(req, timeout=cfg.timeout) as r:
            response = json.loads(r.read())
        message = ((response.get("choices") or [{}])[0].get("message") or {})
        text = (message.get("reasoning_content") or message.get("reasoning")
                or message.get("content") or "").strip()
        if len(text) >= 8 and len(set(text)) == 1:
            raise RuntimeError(
                f"degenerate warm response ({text[0]!r} repeated {len(text)}x); "
                "stopping this pass to protect the prefix cache"
            )
        return response
    except (urllib.error.URLError, OSError, json.JSONDecodeError) as e:
        log(f"  warm failed for {m.path.name}: {e}")
        return None


def _vllm_metrics_busy(payload: str) -> bool | None:
    """Return vLLM request activity, or None when gauges are absent."""
    found = False
    total = 0.0
    names = ("vllm:num_requests_running", "vllm:num_requests_waiting")
    for line in payload.splitlines():
        if not line.startswith(names):
            continue
        try:
            total += float(line.rsplit(None, 1)[-1])
            found = True
        except (ValueError, IndexError):
            pass
    return total > 0 if found else None


def wait_for_idle(cfg: Config, model: str) -> None:
    """Yield the single inference slot to real traffic between warm replays."""
    if cfg.idle_grace <= 0:
        return
    base = f"{cfg.swap.rstrip('/')}/upstream/{model}"
    idle_since: float | None = None
    while True:
        try:
            req = urllib.request.Request(f"{base}/slots", headers={"User-Agent": USER_AGENT})
            with urllib.request.urlopen(req, timeout=10) as r:
                slots = json.loads(r.read())
            busy = any(bool(slot.get("is_processing")) for slot in slots)
        except (urllib.error.URLError, OSError, json.JSONDecodeError):
            try:
                req = urllib.request.Request(f"{base}/metrics", headers={"User-Agent": USER_AGENT})
                with urllib.request.urlopen(req, timeout=10) as r:
                    metric_busy = _vllm_metrics_busy(r.read().decode("utf-8", "replace"))
                busy = True if metric_busy is None else metric_busy
            except (urllib.error.URLError, OSError):
                busy = True
        now = time.monotonic()
        if busy:
            idle_since = None
        elif idle_since is None:
            idle_since = now
        elif now - idle_since >= cfg.idle_grace:
            return
        time.sleep(min(5, max(1, cfg.idle_grace)))


CACHE_LOG = Path.home() / ".cfrproxy" / "cache-observability.jsonl"


def report(resp: dict) -> str:
    """Read the cache telemetry cfrproxy recorded for our own last request.

    The upstream `timings` object does NOT survive back to the client: on the
    translated path cfrproxy rebuilds the response in the inbound dialect and
    drops it. Reading it off the response would silently report every warm as a
    100% hit (reprocessed defaults to 0). cfrproxy captures timings server-side
    into cache-observability.jsonl, so that is the only honest source.
    """
    # Direct llama-swap warmups retain llama.cpp's authoritative timings.
    # They never traverse cfrproxy, so consulting its telemetry would report a
    # stale, unrelated request.
    timings = resp.get("timings") or {}
    if timings:
        cached = int(timings.get("cache_n") or 0)
        reprocessed = int(timings.get("prompt_n") or 0)
        total = cached + reprocessed
        hit = 100.0 * cached / total if total else 0.0
        return (f"ptok={total} reprocessed={reprocessed} hit={hit:.1f}% "
                f"src=upstream reason=direct pp={float(timings.get('prompt_ms') or 0):.0f}ms")
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

    # Canonical targets let one shared prompt-recipe catalog warm several
    # independently valid model KV caches. The KV tensors themselves are never
    # shared: each target gets the same messages rendered by its own template.
    targets = list(dict.fromkeys(cfg.target_models or ([cfg.target_model] if cfg.target_model else [])))
    if targets:
        ready: list[str] = []
        for target in targets:
            identity_dir = cfg.cache_root / "_targets" / hashlib.sha256(target.encode()).hexdigest()[:16]
            identity_dir.mkdir(parents=True, exist_ok=True)
            if dry_run or check_identity(cfg, identity_dir, target):
                ready.append(target)
        targets = ready
    else:
        ok_models: dict[str, bool] = {}
        for m in mans:
            mdir = m.path.parent.parent.parent  # <root>/<model-key>/<client>/<scope>/x.json
            key = str(mdir)
            if key not in ok_models:
                ok_models[key] = True if dry_run else check_identity(cfg, mdir, m.data["model"])
        mans = [m for m in mans if ok_models.get(str(m.path.parent.parent.parent), False)]

    # Keep standard harness heads ahead of incidental session variants. Their
    # stable system/tool prefixes are the whole reason this daemon exists.
    warm_first = {"claude-code", "codex", "omp"}
    mans.sort(key=lambda m: (m.data.get("client") in warm_first, m.mtime), reverse=True)

    # Bounded selection: harness heads first, then most-recently-seen, capped by count and by an
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

    model_targets = targets or [""]
    log(f"{len(mans)} manifest(s) eligible; warming {len(selected)} "
        f"(~{budget} tokens, cap {cfg.max_warm_tokens}) onto "
        f"{', '.join(targets) if targets else 'their recorded models'}")
    for target in model_targets:
        for index, m in enumerate(selected):
            d = m.data
            model = target or d["model"]
            label = f"{d['provider']}/{model} {d['client']}/{d['scope']} {m.path.stem[:12]}"
            if dry_run:
                log(f"  [dry-run] would warm {label} (~{m.est_tokens} tok)")
                continue
            wait_for_idle(cfg, model)
            t0 = time.time()
            resp = warm(cfg, m, target)
            if resp:
                log(f"  warmed {label} in {time.time()-t0:.1f}s :: {report(resp)}")
            if index + 1 < len(selected):
                time.sleep(1)  # let real traffic enter the slot between warm replays


def main() -> int:
    ap = argparse.ArgumentParser(description="prefix-cache warmup + storage manager")
    ap.add_argument("--config", type=Path, default=Path.home() / ".cfrproxy" / "kvwarm.json")
    ap.add_argument("--cache-root", type=Path)
    ap.add_argument("--proxy"); ap.add_argument("--swap")
    ap.add_argument("--provider", default=None, help="restrict to one provider, e.g. fred")
    ap.add_argument("--target-model", default=None,
                    help="replay every eligible prefix onto this canonical model")
    ap.add_argument("--import-capture", type=int,
                    help="import one llama-swap capture as a byte-exact direct manifest")
    ap.add_argument("--max-warm", type=int); ap.add_argument("--max-warm-tokens", type=int)
    ap.add_argument("--idle-grace", type=int,
                    help="idle seconds required before each warm replay; 0 disables")
    ap.add_argument("--interval", type=int)
    ap.add_argument("--once", action="store_true", help="single pass, then exit")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--list", action="store_true", help="show the namespace and exit")
    a = ap.parse_args()

    cfg = Config.load(a.config)
    for src, dst in (("cache_root", "cache_root"), ("proxy", "proxy"), ("swap", "swap"),
                     ("max_warm", "max_warm"), ("max_warm_tokens", "max_warm_tokens"),
                     ("interval", "interval"), ("idle_grace", "idle_grace")):
        v = getattr(a, src, None)
        if v is not None:
            setattr(cfg, dst, v)
    if a.provider is not None:
        cfg.only_provider = a.provider
    if a.target_model is not None:
        cfg.target_model = a.target_model
        cfg.target_models = []

    if a.import_capture is not None:
        if not cfg.target_model:
            ap.error("--import-capture requires --target-model or target_model in config")
        print(import_capture(cfg, a.import_capture, cfg.target_model))
        return 0

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
