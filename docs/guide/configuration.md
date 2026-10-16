# Configuration

Amplio reads its settings once, at startup, from three places: command-line
flags, environment variables, and `config.toml` in the data directory.

## The data directory

Everything amplio owns lives in one directory, chosen by the first of these
that is set:

    --data-dir=/path        flag, per command
    $AMPLIO_DATA_DIR        environment
    ~/.amplio               default

## config.toml

A minimal file needs only the two system models, but it is recommended to
specify the `embed_model` as well.

```toml
system_llm_hq   = "vertex-claude:claude-opus-4-8"
system_llm_fast = "vertex-claude:claude-sonnet-4-6"
embed_model     = "vertex:text-embedding-005"
```

The system LLMs are used for system bookkeeping, such as auto-critic and
compaction. The embed model is used for skill / lesson indexing, if
not set, that sub-system will be disabled. Other useful configs:

| key | default | what it does |
|---|---|---|
| `listen` | `localhost:26759` | bind address for `amplio serve` |
| `db` | `<data-dir>/amplio.db` | SQLite path; useful to point at a faster disk |
| `token` | generated once | web auth token; generated into `auth.token` on first start and reused, so browser sessions survive restarts. Set it here to pin a known value |
| `amplio_bin_paths` | built-in | directories prepended to the agent's `$PATH` |
| `[run] llms` | — | the model menu offered for new runs; the first entry is the default |
| `[skills] dirs` | built-in | skill source directories, layered in order |
| `[skills] blocked` | — | skill names to exclude from every source |
| `[skills] initial` | `5` | how many skills, ranked by relevance to the task, are listed at session start |
| `[skills] pinned` | — | skill dirs whose skills are always listed at session start (below) |
| `[lessons] search` | `true` | let agents search lessons mined from past runs (below) |
| `[bridge.<name>]` | — | a named LLM bridge endpoint ([models](models.md)) |
| `[response_rewrite]` | off | plain-prose restatement of chat conclusions (below) |

Spec syntax for LLM models is in [models](models.md).

### Rewriting chat conclusions

An agent's closing message is written for speed, and it shows: long sentences,
em-dash asides, shorthand. `[response_rewrite]` restates those messages in
plainer prose and shows the result in the chat, with a per-message toggle back
to the agent's own words. The original is always kept; the rewrite is a cache
that can be deleted.

```toml
[response_rewrite]
model = "vertex-gemini:gemini-3.7-flash"   # who does the rewriting
for   = ["opus-5 · xhigh"]                 # which run models opt in
# prompt = "…"                            # optional: replaces the built-in
```

**Opt-in per model, and off by default.** Whether a restatement helps depends on
who wrote the original, so `for` lists the models whose runs get it — matched by
full spec, `#nickname`, or the short label the UI shows, exactly as a bridge
handle names a model. An empty `for` disables the feature.

Only a chatbot session's **conclusions** are rewritten — the final message of a
turn, not the intermediate tool-calling ones. Each costs one extra LLM call, and
the work is fire-and-forget: a failure leaves the message with no toggle. The 
rewrite will be generated async and update on the UI when generation completes.

## Config Precedence

For single values, the first one set wins:

    flag  >  environment  >  config.toml  >  built-in default

| setting | flag | environment |
|---|---|---|
| data directory | `--data-dir` | `$AMPLIO_DATA_DIR` |
| system HQ model | `--system-llm-hq` | `$AMPLIO_SYSTEM_LLM_HQ` |
| system fast model | `--system-llm-fast` | `$AMPLIO_SYSTEM_LLM_FAST` |
| embedding model | `--embed-model` | `$AMPLIO_EMBED_MODEL` |
| skill directories | `--skill-dir` (repeatable) | `$AMPLIO_SKILL_DIRS` (path list) |
| listen address | `--listen` | `$AMPLIO_LISTEN` |
| log level / format | `--log-level`, `--log-format` | `$AMPLIO_LOG_LEVEL`, `$AMPLIO_LOG_FORMAT` |

**Lists replace rather than merge.** Skill directories given by flag replace
those from the environment, which replace those in the file — the highest layer
that is *set* wins wholesale. An explicitly empty list means "none", which is
how you switch skills off; omitting the key entirely means "use the default".

## Optional OTLP/HTTP tracing

Tracing is off by default. Enable it explicitly for `serve` or `headless`:

```bash
OTEL_TRACES_EXPORTER=otlp \
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
./amplio serve
```

Amplio uses the standard Go OpenTelemetry SDK and its OTLP/HTTP protobuf
exporter. The base endpoint above receives requests at `/v1/traces`. For a
collector with a custom path, set `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` to the
complete trace URL instead. Headers and TLS settings use the standard
`OTEL_EXPORTER_OTLP_*` variables; keep authentication headers in the environment.

| Variable | Behavior |
| --- | --- |
| `OTEL_TRACES_EXPORTER` | `otlp` enables export; unset or `none` disables it |
| `OTEL_SDK_DISABLED` | `true` disables initialization even when export is configured |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Full trace URL; takes precedence over the base endpoint |
| `OTEL_EXPORTER_OTLP_HEADERS` | Comma-separated exporter headers; trace-specific headers take precedence |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | Only `http/protobuf` is supported; this is also the default |
| `OTEL_SERVICE_NAME` | Service name, default `amplio` |
| `OTEL_RESOURCE_ATTRIBUTES` | Additional resource attributes |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | SDK sampling configuration |
| `OTEL_BSP_*` | SDK batch size, queue size, schedule delay and export timeout |

Each execution attempt of an agent step starts an independent trace with a
root `amplio.step` span. Model calls (`chat <model>`) and tool executions
(`execute_tool <name>`) are children. Both streaming and non-streaming calls
record model, input/output token counts, cache usage and finish reason using
`gen_ai.*` attributes. Tool failures set error status, including failures
returned to the agent as tool results.

Every span carries `amplio.run.id`, `amplio.session.id`, `amplio.step` and
`gen_ai.agent.name`; child agents also carry `amplio.parent_session.id`.
These IDs correlate traces with the local trajectory and across restarts.
Retries of the same durable step produce separate traces. Idle time between
interactive turns is excluded; a blocking tool such as `await_event` remains
part of its step. Observer and critic activity is outside this initial scope.

Amplio's step, model and tool spans exclude prompts, responses, tool
arguments/results and arbitrary error text. Errors carry a category in
`error.type`. Dependencies that already use OpenTelemetry may also emit spans
through the same provider. Sampling and process crashes can leave gaps in
telemetry; the SQLite event stream remains the durable record.
Export is asynchronous with a bounded queue that drops spans when full, so an
unavailable collector does not block an agent. Shutdown gives pending exports
up to five seconds to drain, including when the run context has been cancelled.

See the [Go exporter configuration](https://opentelemetry.io/docs/languages/go/exporters/)
and [GenAI span conventions](https://github.com/open-telemetry/semantic-conventions-genai/tree/main/docs/gen-ai).

## Recall: skills and lessons

Recall is the agent's search over two corpora: **skills** (documents you supply,
loaded on demand) and **lessons** (mined automatically from past runs). Both are
embedded, so both need `embed_model`:

```toml
embed_model = "vertex:text-embedding-005"

[skills]
dirs    = ["~/skills", "/team/shared/skills"]   # layered; last wins on a name clash
blocked = ["noisy-skill"]
```

Leave `embed_model` empty and amplio starts fine, reporting recall as disabled:

    Recall subsystem (agent skill + knowledge search):
      ✗ skills    disabled — no embed model configured
      ✗ knowledge disabled — no embed model configured

Skills are re-scanned at startup, and their embeddings are cached in the
database, so only new or changed files cost an embedding call.

### What an agent sees at session start

Every session begins with a short list of skills (name and description; the
agent loads a full skill with `recall_load`). By default it is the 5 skills
most relevant to the session's task, ranked across all skill dirs together.
Two keys change that:

```toml
[skills]
dirs    = ["/team/shared/skills", "~/domain-skills"]
initial = 5                    # relevance-ranked skills to list; 0 = none
pinned  = ["~/domain-skills"]  # dirs whose skills are ALWAYS listed
```

**Pinned** skills are listed in every session, whatever the task, in a section
of their own, and they don't count against `initial`: the relevant section is
filled from the remaining skills, so nothing is listed twice. This is for a
small set of skills specific to the problems you work on, which you want every
agent to know about rather than hoping they rank. A pinned dir must also be in
`dirs` (amplio warns and ignores it otherwise). If a pinned skill's name is
overridden by a later, unpinned dir, the skill that wins is not pinned.

Pinning costs context in every session, sub-agents included, so keep the set
small. A chat session has no task to rank against, so it is shown the pinned
skills only.

The lesson list at session start is unaffected by those configs.

### Isolating runs from past lessons

For a controlled experiment you may want runs that cannot inherit anything from
earlier runs. Turn the lesson corpus off for the whole instance:

```toml
[lessons]
search = false
```

or, per invocation:

    amplio serve --lesson-search=false
    AMPLIO_LESSON_SEARCH=0 amplio serve

Lessons are still **mined** at the end of every run, and still browsable by you on the `/recall` page. 
The instance keeps contributing to the corpus, it just stops reading from it. Skills are
unaffected either way.

## The contents of the data directory

What you will find inside:

| path | what it is |
|---|---|
| `config.toml` | the settings below (optional; amplio warns and uses defaults if absent) |
| `amplio.db` | every run, session, event and observation — one SQLite file |
| `artifacts/<run-id>/` | a run's scratch space, created with the run; agents write here via `$AMPLIO_ARTIFACT_DIR` |
| `blobs/<run-id>/` | content-addressed tool-result blobs (e.g. images) kept out of the DB |
| `logs/` | one log file per `serve` invocation |
| `bin/` | `amplio` and `amplio-notify` shims, prepended to the agent's `$PATH` |
| `briefings/` | your own briefings (see [prompt context](prompt-context.md)) |
| `AGENTS.md` | machine-wide operator instructions, if you write one |
| `server.json` | the running server's address, pid and token — how `amplio client` finds it |
| `auth.token`, `lock` | web auth secret; single-owner lock for the directory |
| `cert.pem`, `key.pem` | TLS material, if you set it up ([tls](../internals/tls.md)) |

A data directory is a complete, independent amplio: its own runs, its own
models, its own port. Multiple amplio instances based in different data directories can run
in parallel — see [operations](operations.md).
