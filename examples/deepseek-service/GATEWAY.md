# Multiple inference replicas

Run Recipe once on a separate CPU host and send its rendered token requests to
matching GPU replicas. Mira only forwards the public site's HTTP traffic to the
CPU loopback port. Authentication, model protocols and scheduling belong to
this standalone application.

```text
Codex -> Mira HTTPS port site -> CPU loopback:8006 (key, Recipe, admission, routing)
                                -> GPU A authenticated relay:8007 -> vLLM loopback:8000
                                -> GPU B authenticated relay:8007 -> vLLM loopback:8000
```

The CPU and GPU hosts need ordinary direct network connectivity. Tailscale is
not required. Do not route a replica URL through the same public gateway site.

## GPU replicas

Use the same checkpoint, tokenizer, vLLM version, image plugin, context size and
inference flags on each replica. The complete text/image Python path has been
verified on the official vLLM 0.30.0 image with Recipe 0.1.1 and image plugin
0.1.1. Install `vllm-backend/` in the engine environment and explicitly enable
`VLLM_PLUGINS=deepseek_recipe_images`; retain the loopback-only engine listener.

For the verified 8-GPU configuration, retain tensor parallelism 8, expert
parallelism, Engram CPU offload, FP8 KV cache, prefix caching, async scheduling,
V2 runner, 8 maximum sequences, 4,096 batched tokens and long-prefill threshold,
and the checkpoint's DSPark configuration. Set `--max-model-len 1048576` for a
1M context. The context budget includes input and all output, including thinking.
Validate memory capacity and multimodal compatibility when changing the image
or hardware; setting a context limit does not reserve that many tokens for
every concurrent request.

Install this directory's `requirements.txt` in a separate Python 3.12 environment
on every host. Create a private mode-0600 application key for each relay:

```sh
/path/to/recipe-env/bin/python replica_relay.py \
  --key-file /private/replica/api.key --host 0.0.0.0 --port 8007
```

The relay authenticates before reading the body, exposes only authenticated
health and raw Recipe completions, caps the raw request at 64 MiB, and closes
the engine stream on cancellation. Keep this port on the private inference
network. Public visitors use the CPU frontend; never register an unauthenticated
engine port as a Mira site. The relay does not forward its Bearer key to vLLM.

## CPU gateway

Save a private JSON configuration. The example host names must resolve to the
actual private replica listeners:

```json
{
  "tokenizer": "/models/DeepSeek-V4.1-Flash/tokenizer.json",
  "key_file": "/private/gateway/client.key",
  "alias_key_files": [],
  "context_tokens": 1048576,
  "port": 8006,
  "max_queued": 64,
  "replicas": [
    {"name": "replica-a", "url": "http://gpu-a.internal:8007", "key_file": "/private/gateway/replica-a.key", "capacity": 8, "context_tokens": 1048576},
    {"name": "replica-b", "url": "http://gpu-b.internal:8007", "key_file": "/private/gateway/replica-b.key", "capacity": 8, "context_tokens": 1048576}
  ]
}
```

```sh
/path/to/recipe-env/bin/python gateway.py --config /private/gateway/config.json
```

Run one frontend worker. Two capacity-8 replicas permit 16 admitted requests
and 64 queued requests, with a 30-minute admission wait limit. Health probes
run every two seconds; only healthy replicas with enough configured context
can receive new work. A failed selected request is surfaced to the client and
is never replayed on another replica. Stream closure releases its replica slot.

`alias_key_files` accepts existing client keys during a cutover, without changing
the clients' credentials. Backend credentials are selected independently per
replica. All frontend routes, including `/health/routing`, require a client key.
That diagnostic endpoint reports health, active counts, request/failure counts
and the number of cache hints without exposing addresses, keys or thread IDs.

The gateway rereads replica URLs and their optional `enabled` boolean every two
seconds. Replace the private configuration atomically. Setting `enabled: false`
drains new assignments while existing streams finish; wait for that replica's
`draining: true` and `active: 0` before stopping it. After a replacement becomes
healthy, update its URL and enable it. Address changes clear that replica's
prefix hints. Invalid routing configuration drains new work; diagnostics expose
`configuration_valid: false`. Adding/removing replicas, changing capacities,
context, keys or frontend settings requires an idle gateway restart.

Codex's `thread-id` header scopes affinity to each parent or child independently;
`session-id` is a fallback. The gateway hashes the header and keeps at most
8,192 hints for 30 minutes. A successful SSE completion records a digest of the
actual text-token prefix. Routing weighs estimated uncached input and current
active requests; a full warm replica does not block another available replica.
These are cache-location hints, not proof that vLLM still retains the KV cache.
Image requests use load distribution without text-only prefix hints, and context
eligibility includes their expanded prompt-token count. Gateway restart loses
only these disposable hints; Codex history remains in Mira's PostgreSQL store.

## Request privacy

Run every engine, relay and gateway through `private_runtime.py` from the
container platform, rather than a short-lived Mira SSH session:

```sh
python private_runtime.py --cwd /private/runtime --pid-file /private/runtime/gateway.pid -- \
  /path/to/recipe-env/bin/python /path/to/gateway.py --config /private/gateway.json
```

The launcher discards stdout and stderr in the entire child process tree,
disables Linux/CUDA core dumps, function tracing, debug dump environment settings
and usage telemetry, and supplies a null vLLM logging configuration. Keep vLLM
request/output logging disabled, omit disk KV connectors and explicitly use
`--profiler-config '{"profiler":null}'`. Prefix/KV caches remain volatile memory;
no prompt, completion, image, tool argument, credential or decodable token-ID
sequence is written to inference logs or request storage. Compilation caches
contain code, rather than request tensors. Health endpoints and aggregate metrics
remain available, as does usage in the response sent to the caller. Application
timing diagnostics accept only fixed numeric fields and server-generated IDs;
unknown backend usage/metric extensions cannot enter them.

Disable core dumps on already-running processes before a rollout. Drain active
and queued gateway work before stopping its old process, and drain each replica
before restarting it. Remove old inference log files after their writers stop.
These settings concern inference instances; Mira's authoritative conversation
history and the caller's storage are separate.

An independent checkpoint uses a separate gateway process, client/relay key and
Mira site on the same CPU host. Set the optional `model` field in its JSON config;
never mix different weights in one replica pool. `backend_ready_file` is an
optional absolute admission gate: while absent, new work waits with SSE keepalive
and active streams continue. It can hold a new gateway during a graceful cutover.

## Cutover and verification

Start and authenticate both relays, then the CPU gateway. Verify actual
Responses streams, reasoning deltas, tool round trips, images and cancellation.
Check that both replicas handle requests and all active counts return to zero.
Update the existing Mira port site to the CPU Node's loopback port using its
expected revision. The public URL, registered site and client keys can remain
unchanged.

For a 1M Codex account, set `model_context_window = 1048576`,
`model_auto_compact_token_limit = 940000`, and the model catalog's context and
maximum context to 1,048,576 with 95% effective context. Restart only the
affected idle account processes to load the configuration. Existing thread IDs,
provider names and canonical history remain intact; no new conversation is
required merely to increase the context limit.

Run the application checks with the real tokenizer:

```sh
DEEPSEEK_TEST_TOKENIZER=/models/DeepSeek-V4.1-Flash/tokenizer.json \
  /path/to/recipe-env/bin/python -m unittest discover -s . -p 'test_*.py'
```

Use the container platform's process ownership or a separate application
supervisor. A detached child of Mira SSH can still be reaped when that SSH worker
exits. Mira updates must not own the model or gateway processes. This example
does not provide automatic container rebuild, startup recovery, durable routing
state or multi-gateway coordination; a platform job lifetime is separate from
model readiness and process supervision.
