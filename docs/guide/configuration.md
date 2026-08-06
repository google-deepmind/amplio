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
| `[bridge.<name>]` | — | a named LLM bridge endpoint ([models](models.md)) |

Spec syntax for LLM models is in [models](models.md).

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
