# Local DeepSeek for a Mira Codex account

This is a standalone model application. It uses official
[DeepSeek Recipe](https://github.com/deepseek-ai/deepseek-recipe) 0.1.1 for prompt
encoding and protocol parsing, and an existing vLLM engine for inference. Mira
does not import this application or interpret model requests. Visitor Bearer
authentication belongs here; Mira supplies its ordinary persistent port site.

```text
Mira-managed Codex on an execution Node
  -> HTTPS port site (generic Mira byte transport)
  -> this application on the model Node, loopback:8001 (API key + Recipe)
  -> vLLM loopback:8000 (raw token IDs; image extension for multimodal input)
  -> GPU inference
```

## Model service

Use a separate Python 3.12 virtual environment. Install `requirements.txt`; do
not change the environment of an already running inference engine.

```sh
python3 -m venv /path/to/recipe-env
/path/to/recipe-env/bin/pip install -r requirements.txt
```

Create a random application key in a private directory with mode 0600. Pass its
file path to the service; never put the key in arguments or commit it. Use the
tokenizer belonging to the actual checkpoint. The model must be served as
`DeepSeek-V4.1-Flash`, with a 294,912-token total context in this example.

```sh
/path/to/recipe-env/bin/python service.py \
  --tokenizer /path/to/DeepSeek-V4.1-Flash/tokenizer.json \
  --key-file /private/deepseek/api.key \
  --backend http://127.0.0.1:8000 \
  --port 8001
```

Run this under the model host's existing process supervisor, independently of
Mira Node and SSH sessions. A detached tmux session is an option inside a
container, but does not provide boot recovery or restart after a process crash.
Keep the inference engine on loopback. Only register the authenticated frontend
port with a Mira site. The raw loopback backend remains available to trusted
processes on that host and trusted Mira administrators.

The vLLM backend must support streaming `/v1/completions` with integer prompts,
`return_token_ids=true` and continuous usage statistics. Do not route through its
Chat Completions renderer: Recipe has already rendered the complete prompt.
EOS token IDs from vLLM count toward usage but are not emitted as answer text.
No model request is automatically retried by this application.

Codex v2 subagent tasks, follow-ups, messages and completion reports arrive as
`agent_message` input items. Recipe 0.1.1 does not recognize that type and would
silently discard it. The frontend converts plaintext agent items into ordinary
user messages before Recipe conversion, preserving sender, recipient, content
blocks and their position in history. These messages consume the normal context
budget. Empty, malformed or encrypted agent content returns 400 before inference;
the frontend never drops a task or partially forwards a mixed encrypted message.
This is a model-input projection; Codex's native history remains unchanged.

### Image backend

For images, install the small `vllm-backend/` endpoint plugin in the **vLLM**
environment, then restart that engine using its existing launch parameters:

```sh
/path/to/vllm-env/bin/pip install --no-deps ./vllm-backend
VLLM_PLUGINS=deepseek_recipe_images /path/to/vllm-env/bin/vllm serve ... --host 127.0.0.1
```

Preserve any other required plugin names in `VLLM_PLUGINS`. Endpoint plugins
require explicit opt-in; this plugin refuses a non-loopback listener. It uses
vLLM's endpoint-plugin and serving interfaces, verified with
`0.30.1rc1.dev709+g21d93d0d8.cu129`. Other builds need compatibility validation.
No installed vLLM source file is patched. Adding the plugin requires a planned
engine restart/model reload; retain the prior launch command for rollback.

With plugin 0.1.1, pass `--recipe-backend` to the frontend to use that same private
endpoint for text. It enables per-request timing and cached-token details on its
own serving instance. The frontend consumes vLLM's final usage-only frame before
completion; cache counts are not inferred from process-wide metrics. Older
conversations that recorded zero cached tokens cannot be corrected retroactively
without per-request evidence. Plain text `/v1/completions` remains available when
the flag is omitted.

Each completed or interrupted call logs one `inference_timing` record with only
counts, durations and backend metrics. `prepare_ms`, `backend_first_token_ms`,
`first_reasoning_ms`, and `first_answer_ms` are measured from frontend request
handling; they are not client round-trip timings. Prompts, images, outputs and
credentials are excluded. The authenticated frontend keeps idle HTTP connections
for 30 minutes to match Mira's connection reuse; no requests are replayed.

The frontend accepts inline PNG, JPEG, WebP and GIF images as data URLs in
Responses or Chat Completions, including images returned by client tools.
External image URLs are rejected. Recipe performs image validation, alpha
handling, resizing and WebP preprocessing using its official limits. GIF input
uses a static frame; this is not video support. `detail=low` applies Recipe's
512-pixel limit before token-budget fitting; other levels still undergo its
normal resizing and the 1,024-token-per-image budget.

The checkpoint spells Recipe's `<｜image｜>` placeholder as
`<｜deepseek_image｜>`, both representing token 129264. The adapter checks this
mapping and sends one sentinel per image, with the processed images in order,
to private `/recipe/v1/completions`. The plugin calls vLLM's normal multimodal
processor with those raw tokens, preserving Recipe's conversation/tool encoding.
It checks that the expanded prompt length equals Recipe's image-aware count
before inference. Image tokens therefore consume context and appear in usage;
they are never silently dropped. The same stream parser handles text and images.

The entrypoint authenticates **all** HTTP paths before reading a request body.
It serves `/v1/responses`, `/v1/chat/completions`, `/v1/models`, and `/health`.
Raw completions, metrics, tokenizer APIs, API documentation and other engine
routes are not exposed. At most four authenticated requests are active, with a
32 MiB request-body limit and a 600-second backend read-idle timeout. Streaming
uses bounded HTTP buffers; disconnects close the upstream inference request,
including non-streaming calls. An interrupted backend stream emits an error,
never a fabricated successful completion.

Defaults retain this deployment's sampling settings: temperature 1.0, top-p
0.95, thinking enabled, effort `max` and up to 32,768 output tokens. Explicit
effort values follow Recipe's V4.1 mapping: `low=50`, `high=75`, `max=100`;
`none` disables thinking. The output budget includes reasoning and is limited
by the remaining context. An explicit budget that does not fit returns 400.

## Isolated Codex account

Follow the [official DeepSeek Codex guide](https://api-docs.deepseek.com/quick_start/agent_integrations/codex/).
Save its `models.json` catalog as a local input file. Avoid running the official
one-click script against a shared `~/.codex`; this helper preserves the official
instruction templates while adapting metadata for this local deployment:

```sh
python3 prepare_codex.py \
  --official-catalog /path/to/official-models.json \
  --codex-home /private/deepseek/codex \
  --base-url https://p-model.preview.example.test/v1
```

The destination must not already exist. The generated home has the actual model
name, text and image input, the 294,912 context, a 240,000 auto-compaction threshold,
the official freeform `apply_patch` tool, SSE Responses and low/high/max effort
choices. It defaults to max, preserving strength 100. Both HTTP and stream
retries are disabled so failures remain visible during validation.

Put `DEEPSEEK_API_KEY=<application key>` in a private mode-0600 environment file.
When a system proxy is present, set `NO_PROXY` and `no_proxy` for the exact model
site host. Create a **custom / providerConfig** Mira account adopting this Codex
home, configure that account's `environmentFiles`, select the complete existing
Mira-compatible Codex package, and start it. Model credentials stay on the
execution Node. Account configuration and model authentication do not change
Mira identities, visitor authentication, or PostgreSQL conversation ownership.

Mira defaults to four simultaneous Codex accounts. If they are all occupied,
set `MIRA_NODE_MAX_CODEX_RUNTIMES` in the existing Node service environment and
perform a planned service restart. Preserve other accounts and their desired
running states. Restarting the execution Node can interrupt its active agents.

## Validation and limits

Run the deterministic contract checks with the real model tokenizer:

```sh
DEEPSEEK_TEST_TOKENIZER=/path/to/model/tokenizer.json \
  /path/to/recipe-env/bin/python -m unittest -v
# In vllm-backend/, using the pinned engine environment (no GPU load):
/path/to/vllm-env/bin/python -m unittest -v
```

Then validate against actual inference and the managed Codex runtime: text,
developer instructions, named and namespaced functions, custom `apply_patch`,
tool output round trips, SSE ordering, context limits, missing/wrong/correct
keys, cancellation, cold account restart and resume, and a read/edit/test task.
Include image OCR/color recognition, ordered multiple images, image-bearing
tool results, text regression, and cancellation of an image inference request.
Also verify a fresh Codex subagent with no inherited history, an idle follow-up,
queued messages and child completion reports using distinct task markers. A
successful collaboration tool receipt alone does not prove model visibility.
Use a disposable workspace for edits. Compare native vLLM and Recipe on model
host loopback before attributing domain failures to either protocol adapter.

The initial comparison used vLLM `0.30.1rc1.dev709+g21d93d0d8.cu129`, Recipe
source `8cadfede7063c896b944e7bae05daa3549ae97ea` / package 0.1.1, and Codex
`0.159.3-mira.1`. Native vLLM completed ordinary and namespaced function round
trips, but returned `function_call` for a declared custom `apply_patch`; one
developer-message case completed without a final answer. Those observations
justify using Recipe at the application boundary; they are not claims about
all vLLM versions or all prompts.

This deployment supports text and inline images. It does not fetch external
image URLs, execute tools,
store Responses conversations, implement `previous_response_id`, background
jobs, remote compaction, encrypted reasoning, or built-in web search. Clients
send full history; Codex executes tools and Mira persists its native history.
Recipe 0.1.1 reports total output tokens including reasoning, but hard-codes the
Responses reasoning-token breakdown to zero; do not interpret that field as
the measured thinking count. Recipe also does not enforce tool JSON Schema
strictness or constrained-output JSON Schema. Refer to its upstream supported
scope before adding clients or advertising capabilities.

Deploy by starting and testing the frontend on a new loopback port, then update
the existing Mira site's target using its expected revision. Preserve the
engine's model and inference settings. Roll back the frontend code/configuration
to a known authenticated version; pointing a public site back to an
unauthenticated raw engine would remove application authentication.
