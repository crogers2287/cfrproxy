# Caveman compression of tool results

Opt-in per provider (`--caveman` / the provider dialog) and per share endpoint. Tool results
older than the newest few turns are reduced by type — logs keep their errors, head and tail;
JSON keeps its structure; code keeps signatures — with an explicit marker where content was
elided, so the model knows it is reading an abridgement.

Two rules it will not break, because both were learned on this box:

1. **Prefix-cache safety.** Compression is deterministic per message, independent of the rest of
   the conversation, so the cached prefix is never rewritten as a conversation grows.
2. **Never touch the stable head.** System prompts and tool schemas are left alone; only
   `tool`-role results are compressed.

Per request, a client can force it with `X-Caveman: 1` or a `caveman: true` body field. When a
provider answers with a context-overflow error, cfrproxy also compresses and retries once before
walking the fallback chain. The trace records messages compressed and bytes before/after.
