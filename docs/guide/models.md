# LLM Providers

Amplio resolves a model from a **spec string**:

```
<provider>[{k=v&k=v…}]:<model>[?k=v&k=v…][#nickname]
   ^          ^              ^                ^
   |          |              |                display label, never sent anywhere
   |          |              model id, plus MODEL args passed through verbatim
   |          CLIENT args: the ones amplio interprets
   registry key
```

One rule decides which side an argument goes on:

> **The block holds arguments amplio interprets. The query holds arguments
> passed through untouched.**

Note what that does *not* say: interpreted arguments frequently do reach the API
— `cache_ttl=1h` becomes Anthropic's `cache_control`, `profile=litellm` sets
several body fields. What separates them is who owns the *meaning*. The test for
a new argument: **would it still mean the same thing pointed at a different
provider?** If yes it is amplio's vocabulary and belongs in the block; if it is
the upstream API's own field name, it belongs in the query and amplio never
looks at it.

```
vertex-claude{cache_ttl=1h}:claude-opus-5?thinking.type=adaptive
openai{base_url=http://localhost:4000/v1&profile=litellm}:claude?reasoning.effort=high
vertex-gemini:gemini-3.7-flash?thinking_level=high
```

A provider with nothing to configure needs no block, which is why most specs
don't have one.

`llm.ParseSpec` (internal/llm/spec.go) is the only parser. Values are
percent-encoded where needed — `:` and `/` stay literal so URLs read normally,
but a literal `}` is `%7D` and a literal `#` is `%23`. `Spec.String()` renders a
canonical form (keys sorted, minimal escaping) for *matching* — menu lookup,
dedup, cache keys — never for display, since it may reorder what the operator
typed.

## Providers

| Prefix          | Family / backend                                 | Auth                                  |
| --------------- | ------------------------------------------------ | ------------------------------------- |
| `vertex-claude` | Claude on Vertex AI (`anthropic-sdk-go`)         | ADC + `VERTEXAI_PROJECT`/`VERTEXAI_LOCATION` |
| `claude`        | Claude direct Anthropic API (`anthropic-sdk-go`) | `ANTHROPIC_API_KEY`; no GCP |
| `vertex-gemini` | Gemini on Vertex AI (`google.golang.org/genai`)  | ADC + `VERTEXAI_PROJECT`/`VERTEXAI_LOCATION` |
| `gemini`        | Gemini Developer API (`genai`)                   | `GEMINI_API_KEY` (or `GOOGLE_API_KEY`); no GCP |
| `openai`        | **any** OpenAI-compatible `/v1/chat/completions` | `OPENAI_API_KEY` (or `{api_key_env=…}`); none for a local server |
| `subprocess`    | out-of-process bridge over a unix socket         | whatever the bridge needs    |

All providers default to `MaxTokens` (output cap) **65536**. The Claude provider
always streams under the hood (even for blocking `Call`s) because Anthropic
rejects non-streaming requests whose `max_tokens` could exceed the 10-minute
limit.

Specs are configured in `config.toml` (`[run] llms`, `system_llm_hq`,
`system_llm_fast`) — see [config.toml](#example-configtoml).

## Thinking control

There is **no unified thinking interface** — thinking knobs differ by model
generation, so the spec args are passed through per family. The model itself
validates them, so a wrong knob returns a clear API error (e.g. opus-4-8 rejects
`thinking.type=enabled` with *"…not supported for this model. Use
thinking.type.adaptive and output_config.effort…"*).

### Claude (`vertex-claude:`)

Args are injected **verbatim** into the request body as raw JSON, using dotted
paths (so `thinking.budget_tokens` nests). Values are coerced to int → bool →
float → string. Use whatever the Anthropic API supports for that model; no
amplio code change is needed when Anthropic adds a knob.

- **Newer models — opus-4-7, opus-4-8** use *adaptive* thinking plus an effort
  level (explicit `budget_tokens` is **rejected**):

  ```
  vertex-claude:claude-opus-4-8?thinking.type=adaptive&output_config.effort=high
  ```

  `output_config.effort` ∈ `low | medium | high | xhigh | max`.

- **Older models — opus-4-6, sonnet-4-6** use *enabled* thinking with an explicit
  token budget (they also accept adaptive):

  ```
  vertex-claude:claude-opus-4-6?thinking.type=enabled&thinking.budget_tokens=2048
  ```

  `budget_tokens` must be ≥ 1024 and < `max_tokens`.

### Gemini (`vertex-gemini:` / `gemini:`)

`genai` is a typed SDK (no raw passthrough), so a **known key set** is mapped;
an unknown key fails fast at construction:

| arg                | maps to                          |
| ------------------ | -------------------------------- |
| `thinking_level`   | `ThinkingConfig.ThinkingLevel` (`minimal`, `low`, `medium`, `high`; unset = the model's own default) |
| `include_thoughts` | `ThinkingConfig.IncludeThoughts` (bool; default `true`) |
| `temperature`      | `GenerateContentConfig.Temperature` (float) |

```
vertex-gemini:gemini-3.7-flash?thinking_level=low&include_thoughts=true
```

## Verified configurations

Tested live (Vertex), single-turn **and** multi-turn with tool calls:

| spec                                                                          | thinking |
| ----------------------------------------------------------------------------- | -------- |
| `vertex-claude:claude-opus-4-8?thinking.type=adaptive&output_config.effort=high` | adaptive + effort |
| `vertex-claude:claude-opus-4-7?thinking.type=adaptive&output_config.effort=high` | adaptive + effort |
| `vertex-claude:claude-opus-4-6?thinking.type=enabled&thinking.budget_tokens=2048` | enabled + budget |
| `vertex-claude:claude-sonnet-4-6?thinking.type=enabled&thinking.budget_tokens=2048` | enabled + budget |
| `vertex-gemini:gemini-3.7-flash?thinking_level=low`                            | level |
| `vertex-gemini:gemini-3.7-flash?thinking_level=high`                           | level |
| `vertex-gemini:gemini-3.6-flash?thinking_level=medium`                         | level |
| `vertex-gemini:gemini-3.1-pro-preview?thinking_level=high`                     | level |

## OpenAI-compatible endpoints (`openai:`)

The `openai:` provider is not really "OpenAI support" — it speaks the
de-facto lingua franca of the ecosystem, so **one** provider reaches the hosted
OpenAI API, vLLM, SGLang, llama.cpp, Ollama, LM Studio, OpenRouter, DeepSeek,
Groq, Together, Fireworks, a LiteLLM proxy, or a corp gateway. Point it
somewhere with `base_url`:

```
openai:gpt-5.6?reasoning.effort=high
openai{base_url=http://localhost:4000/v1&profile=litellm}:claude
openai{base_url=http://localhost:11434/v1&profile=ollama}:qwen3:30b-a3b
```

(The model id may contain colons — the spec splits on the **first** one only.)

### Client-only args

These configure the client and are never sent in the request body, so they go in
the `{…}` block:

```
openai{base_url=http://localhost:11434/v1&profile=ollama}:qwen3.5
```

An unknown key **in the block** is an error naming what this provider accepts —
the client owns that namespace, so a typo is catchable. Keys in the query are
never validated: the server is the authority on what it accepts.

| arg | default | meaning |
| --- | --- | --- |
| `base_url` | `$OPENAI_BASE_URL`, else `https://api.openai.com/v1` | the endpoint |
| `api_key_env` | `OPENAI_API_KEY` | *which env var* holds the key, so several endpoints coexist in one config without secrets in `config.toml`. A missing key is fine for local servers |
| `profile` | `openai` if `base_url` is the default, else `generic` | preset for the two knobs below |
| `max_tokens_field` | per profile | `max_tokens` vs `max_completion_tokens` |
| `stream_usage` | per profile | send `stream_options.include_usage` |
| `capture_extras` | `false` | keep non-standard reasoning containers on the response's `ProviderExtra` (not replayed; see below) |
| `max_tokens` | 65536 | output cap. Accepted by **every** provider (handled centrally), and written into whichever field the profile selects — so it can no longer collide with `max_completion_tokens` |

### Profiles

Deviations cluster by server implementation, so name your server rather than
assembling knobs. Any explicit arg overrides the preset.

A preset is a *claim about a server*, so each row says what it was checked
against. An unverified row is a guess that happens to be written down.

| profile | `stream_usage` | `max_tokens_field` | verified against |
| --- | --- | --- | --- |
| `openai` | true | `max_completion_tokens` | gpt-5.4-nano — rejects `max_tokens` outright |
| `litellm` | true | `max_tokens` | LiteLLM proxy → Vertex (claude, gemini) |
| `vllm` | true | `max_tokens` | **not verified** — assumed to match `litellm` |
| `ollama` | true | `max_tokens` | ollama + qwen3.5 — accepts *and honours* `stream_options` |
| `generic` | false | `max_tokens` | n/a — deliberately minimal; some servers 400 on unknown fields |

Getting a row wrong is quiet rather than loud: `ollama` originally shipped with
`stream_usage: false`, so streamed turns reported zero tokens until it was
measured. Verify a new row with the live suite (below) before trusting it, and
prefer `generic` plus explicit args over a preset you haven't checked.

### Everything else is passthrough

Any other arg is injected into the request body via dotted paths, coerced to
its natural JSON type — the same convention as `vertex-claude`, so a
server-specific knob never needs a code change:

```
reasoning.effort=high     →  {"reasoning": {"effort": "high"}}
provider.order=cerebras   →  {"provider": {"order": "cerebras"}}   (OpenRouter routing)
max_tokens=4096           →  overrides the default output cap
```

### Known limits

- **Thinking/reasoning is displayed, not replayed.** Reasoning text is read from
  `reasoning_content` or `reasoning` into the agent's Thoughts, but the
  provider-specific *signatures* are not echoed back on later turns (contrast
  the native `vertex-claude` / `vertex-gemini` paths, which do replay them).
  Expect slightly weaker multi-turn tool use from thinking models through this
  provider. `capture_extras=true` persists the containers for a future replay
  implementation.
- **Images on tool results** are re-emitted as a following `user` turn, because
  a `tool` message must carry a plain string.
- **Azure OpenAI is not supported here** — it needs `api-version`, an `api-key`
  header and deployment names in the path. Point `base_url` at a LiteLLM proxy,
  which speaks Azure.

### Verifying a new server

The test corpus under `internal/llm/openai/testdata/` holds **real captures**
(currently four genuinely different dialects: OpenAI splits tool arguments
mid-KEY, LiteLLM→Claude splits mid-value, LiteLLM→Gemini sends whole calls, and
ollama sends whole calls while repeating `role` on every frame and putting
reasoning under `reasoning`). Before trusting a new server, run the live suite
against it:

```bash
AMPLIO_OPENAI_TEST_BASE_URL=http://localhost:4000/v1 \
AMPLIO_OPENAI_TEST_MODEL=claude \
AMPLIO_OPENAI_TEST_PROFILE=litellm \
  make test-integration
```

It exercises a plain call, parallel tool calls over a live stream, and a
tool-result round trip — the three shapes where servers actually diverge. If it
passes, capture its SSE and add it to `testdata/` so the dialect stays pinned.

## Bridges

A **bridge** is any process that speaks amplio's own NDJSON protocol
(`internal/llm/bridge`), which lets amplio use a backend it cannot link in —
another API family, an internal service, a model on a machine that holds the
credentials. The bridge can be written in any language; `bridges/bridge.py` is a
stdlib-only reference with an `echo` backend, and
[bridges/README.md](../../bridges/README.md) documents the protocol.

There are two modes, differing in who owns the process: `subprocess:` — 
amplio owns the process, e.g.

```
subprocess{bin=/path/to/bridge.py}:echo-model?backend=echo
```

Amplio spawns the bridge and manages its lifecycle: one long-lived process per
spec (reused across runs), crash-restart with a one-shot retry, health-poll
readiness, graceful reap on exit (plus `Pdeathsig` on Linux). Its stdout/stderr
go to the log at debug level, and its **last 50 stderr lines are quoted in the
error** when it fails to start or exits unexpectedly — otherwise that diagnosis
is invisible at the default log level.

`bin=` is the only client arg. The rest of the spec — client block and query
alike — is forwarded verbatim to the bridge as `$AMPLIO_BRIDGE_SPEC`, because
from amplio's side a bridge is opaque: we cannot know which of its knobs
configure the client and which configure the model. That whole spec is also the
reuse key, so two specs differing in any argument get their own process.

`bridge:` — amplio dials a server, e.g.

```
bridge{url=http://127.0.0.1:26760}:model-name?temperature=0.2
bridge{url=unix:///tmp/bridge.sock}:model-name
```

Amplio makes requests to a bridge someone else is running. The underlying
protocol is the same for `bridge` and `subprocess` modes.



## Example config.toml

```toml
system_llm_hq   = "vertex-claude:claude-opus-4-8?thinking.type=adaptive&output_config.effort=high"
system_llm_fast = "vertex-gemini:gemini-3.5-flash?thinking_level=minimal"  # least thinking = fastest

[run]
llms = [
  "vertex-claude{cache_ttl=1h}:claude-opus-4-8?thinking.type=adaptive&output_config.effort=high",
  "vertex-claude:claude-opus-4-6?thinking.type=enabled&thinking.budget_tokens=4096",
  "vertex-gemini:gemini-3.1-pro-preview?thinking_level=high",
]
```

## Embedding providers

Recall (skill + lesson similarity search) needs a text embedder, configured
separately via `embed_model` (or `--embed-model` / `$AMPLIO_EMBED_MODEL`). 
The spec mirrors the LLM convention: `<backend>:<model>`.

`createEmbedder` (cmd/amplio) splits on the first `:` to pick the backend. A
bare model name with no `:` defaults to the `vertex` backend (back-compat with
older `embed_model` values).

| Backend  | Family / backend                            | Auth                                           |
| -------- | ------------------------------------------- | ---------------------------------------------- |
| `vertex` | Vertex AI embeddings (`google.golang.org/genai`) | ADC + `VERTEXAI_PROJECT`/`VERTEXAI_LOCATION` |
| `gemini` | Gemini Developer API embeddings (`genai`)   | `GEMINI_API_KEY` (or `GOOGLE_API_KEY`); no GCP |
| `openai` | **any** OpenAI-compatible `/v1/embeddings`  | `OPENAI_API_KEY` (or `{api_key_env=…}`); none for a local server |

The `openai` backend takes the same `base_url` / `api_key_env` args as the chat
provider, which is what lets a **self-hosted** deployment use recall at all:

```toml
embed_model = "openai{base_url=http://localhost:4000/v1}:text-embedding-005"
```

Note the cache-key subtlety: the same model name at a different endpoint is a
different embedding space, so a non-default `base_url` contributes a short hash
to the cache key (`openai_text-embedding-005@a4843d`). Vectors from two servers
can never collide.

**Model availability differs by backend.** For example `text-embedding-005` is
Vertex-only, while `gemini-embedding-001` is available on both. Anthropic has no
embedding models, so there is no `claude` embedder backend.

The resolved `<backend>_<model>` is the **cache key** for stored vectors
(skills + lessons). If `embed_model` is unset, recall is disabled (no embedder is built).

```toml
embed_model = "vertex:text-embedding-005"   # or: gemini:gemini-embedding-001
```
