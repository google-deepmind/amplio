# LLM Providers

Amplio resolves a model from a **spec string** of the form:

```
<provider>:<model>[?k=v&k=v…]
```

`createProvider` (cmd/amplio) splits on the first `:` to pick the provider
"class" from the registry, then splits the remainder on `?` into the model id and
an optional query of per-model **args** that are forwarded to the request. Each
provider family interprets the args (see [Thinking](#thinking-control) below).

## Providers

| Prefix          | Family / backend                                 | Auth                                  |
| --------------- | ------------------------------------------------ | ------------------------------------- |
| `vertex-claude` | Claude on Vertex AI (`anthropic-sdk-go`)         | ADC + `VERTEXAI_PROJECT`/`VERTEXAI_LOCATION` |
| `claude`        | Claude direct Anthropic API (`anthropic-sdk-go`) | `ANTHROPIC_API_KEY`; no GCP |
| `vertex-gemini` | Gemini on Vertex AI (`google.golang.org/genai`)  | ADC + `VERTEXAI_PROJECT`/`VERTEXAI_LOCATION` |
| `gemini`        | Gemini Developer API (`genai`)                   | `GEMINI_API_KEY` (or `GOOGLE_API_KEY`); no GCP |
| `openai`        | **any** OpenAI-compatible `/v1/chat/completions` | `OPENAI_API_KEY` (or `?api_key_env=`); none for a local server |
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
| `thinking_budget`  | `ThinkingConfig.ThinkingBudget` (int; `0` disables where supported, `-1` dynamic) |
| `include_thoughts` | `ThinkingConfig.IncludeThoughts` (bool; default `true`) |
| `temperature`      | `GenerateContentConfig.Temperature` (float) |

```
vertex-gemini:gemini-3.5-flash?thinking_budget=2048&include_thoughts=true
```

## Verified configurations

Tested live (Vertex), single-turn **and** multi-turn with tool calls:

| spec                                                                          | thinking |
| ----------------------------------------------------------------------------- | -------- |
| `vertex-claude:claude-opus-4-8?thinking.type=adaptive&output_config.effort=high` | adaptive + effort |
| `vertex-claude:claude-opus-4-7?thinking.type=adaptive&output_config.effort=high` | adaptive + effort |
| `vertex-claude:claude-opus-4-6?thinking.type=enabled&thinking.budget_tokens=2048` | enabled + budget |
| `vertex-claude:claude-sonnet-4-6?thinking.type=enabled&thinking.budget_tokens=2048` | enabled + budget |
| `vertex-gemini:gemini-3.5-flash?thinking_budget=2048&include_thoughts=true`    | budget |
| `vertex-gemini:gemini-3.1-pro-preview?thinking_budget=2048&include_thoughts=true` | budget |

## OpenAI-compatible endpoints (`openai:`)

The `openai:` provider is not really "OpenAI support" — it speaks the
de-facto lingua franca of the ecosystem, so **one** provider reaches the hosted
OpenAI API, vLLM, SGLang, llama.cpp, Ollama, LM Studio, OpenRouter, DeepSeek,
Groq, Together, Fireworks, a LiteLLM proxy, or a corp gateway. Point it
somewhere with `base_url`:

```
openai:gpt-5.6?reasoning.effort=high
openai:claude?base_url=http://localhost:4000/v1&profile=litellm
openai:qwen3:30b-a3b?base_url=http://localhost:11434/v1&profile=ollama
```

(The model id may contain colons — the spec splits on the **first** one only.)

### Client-only args

These configure the client and are never sent in the request body:

| arg | default | meaning |
| --- | --- | --- |
| `base_url` | `$OPENAI_BASE_URL`, else `https://api.openai.com/v1` | the endpoint |
| `api_key_env` | `OPENAI_API_KEY` | *which env var* holds the key, so several endpoints coexist in one config without secrets in `config.toml`. A missing key is fine for local servers |
| `profile` | `openai` if `base_url` is the default, else `generic` | preset for the two knobs below |
| `max_tokens_field` | per profile | `max_tokens` vs `max_completion_tokens` |
| `stream_usage` | per profile | send `stream_options.include_usage` |
| `capture_extras` | `false` | keep non-standard reasoning containers on the response's `ProviderExtra` (not replayed; see below) |

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

## Subprocess bridges (`subprocess:`)

For backends whose SDKs shouldn't be linked into the main binary (e.g.
corp-only API), `subprocess:/path/to/bridge?model=NAME[&k=v]`
spawns an out-of-process bridge and talks to it over a tiny HTTP/NDJSON protocol
on a Unix domain socket. amplio manages the lifecycle: one long-lived process
per spec (reused across runs), crash-restart with a one-shot retry, health-poll
readiness, graceful reap on exit (+ `Pdeathsig` on Linux).

The bridge can be written in any language; `bridges/bridge.py` is a stdlib-only
reference with an `echo` backend (used for smoke testing) — see
[bridges/README.md](../bridges/README.md) for the protocol and how to add
your own backend. Example spec for the bundled echo backend:

```
subprocess:/path/to/bridge.py?model=echo-model&backend=echo
```



## Example config.toml

```toml
system_llm_hq   = "vertex-claude:claude-opus-4-8?thinking.type=adaptive&output_config.effort=high"
system_llm_fast = "vertex-gemini:gemini-3.5-flash?thinking_budget=0"   # 0 = no thinking (fast)

[run]
llms = [
  "vertex-claude:claude-opus-4-8?thinking.type=adaptive&output_config.effort=high",
  "vertex-claude:claude-opus-4-6?thinking.type=enabled&thinking.budget_tokens=4096",
  "vertex-gemini:gemini-3.1-pro-preview?thinking_budget=4096",
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
| `openai` | **any** OpenAI-compatible `/v1/embeddings`  | `OPENAI_API_KEY` (or `?api_key_env=`); none for a local server |

The `openai` backend takes the same `base_url` / `api_key_env` args as the chat
provider, which is what lets a **self-hosted** deployment use recall at all:

```toml
embed_model = "openai:text-embedding-005?base_url=http://localhost:4000/v1"
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
