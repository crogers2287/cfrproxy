# Model pools: one name, several instances

`model_pools` (a JSON setting, editable with `cfrproxy config set model_pools '…'` or the admin
API) maps a logical model to the upstream instances that serve it:

```json
{"tiel-w6800": ["tiel-coder-q5-w6800", "tiel-b-w6800"]}
```

A request for `tiel-w6800` goes to whichever member has the fewest requests in flight; llama.cpp
queues it there if every member is busy. Members are the same weights on different cards, so two
instances roughly double aggregate throughput while each stream keeps single-card speed (a
layer-split single instance measured 1.7× slower under load).

## Affinity pools

The object form enables prefix-affine routing, slot probing, and sibling failover:

```json
{"ornith": {"members": ["ornith-kvx-w6800", "ornith-kvx-w6800-b"], "affinity": true, "probe": true, "failover": true}}
```

- **affinity** — a live conversation always returns to the instance holding its KV; a new
  conversation prefers the instance that has served its static prefix before, unless that
  instance is much deeper in work; an unseen prefix is placed by load.
- **probe** — consult the upstream's `/slots` view for cold placement.
- **failover** — the sibling instances are tried before any other fallback, since they are the
  same model; this is not gated by `no_fallback`.

The pooled name is advertised in `/v1/models` and the trace notes which member served the
request and why (`pool→ornith-kvx-w6800-b (affinity)`).
