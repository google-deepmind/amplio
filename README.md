# Amplio: A Lightweight Agent Harness for Robust and Long-Horizon Runs

* **Lightweight**: Multi-agent framework based on a simple step model that provides natural crash-resume capability, and agent / user / env message coordination. Agents work through a small set of generic tools: shell, file edit, sub-agent spawn, and inter-agent coordination.
* **Robust**: DB-first persistence; crashing at any point during the agentic loop can be robustly resumed.
  * The agentic loop is guaranteed to be consistent, failures in the tool / environment are not handled by the harness, but reported to the agent.
  * Sub-agent session trees are automatically resumed upon server recovery.
* **Autonomous**: Each run can be fully driven by the autonomous agent, or cooperatively driven with user interactions.

![Amplio runs list](docs/guide/images/amplio-main.webp)

## Installation



Amplio is a single binary with an embedded frontend. To build it yourself you need Go (see `go.mod` for the version) and Node.js 22+:

```bash
make build
```

## Quick start

Amplio needs two things: a **data directory** (a local SQLite database and
everything else it owns) and an **LLM provider**.

Write `~/.amplio/config.toml`:

```toml
# For bookkeeping: run reports, summaries, compaction.
system_llm_hq   = "vertex-gemini:gemini-3.1-pro-preview"
system_llm_fast = "vertex-gemini:gemini-3.6-flash"

# Enables skill + lesson search. Recommended.
embed_model     = "vertex:text-embedding-005"

[run]  # The model menu in the new-run form.
llms = [
  "vertex-claude{cache_ttl=1h}:claude-opus-5?thinking.type=adaptive&thinking.display=summarized",
  "vertex-gemini:gemini-3.6-flash",
]
```

Please consult [GCP prompt caching doc](https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/partner-models/claude/prompt-caching)
before using the `{cache_ttl=1h}` control on the `vertex-claude` provider.
For Vertex AI, point ADC at your GCP project:

```bash
export VERTEXAI_PROJECT=<your-gcp-project>
export VERTEXAI_LOCATION=global
gcloud auth application-default login
```

Then start the server and open the URL it prints:

```bash
amplio serve
```

From there, describe a task in the composer and start a run.

## Guide

| Doc | Summary |
|---|---|
| [Runs](docs/guide/runs.md) | autonomous and interactive runs, sub-agents, reports and grades |
| [Configuration](docs/guide/configuration.md) | the data directory, `config.toml`, precedence, recall |
| [Models](docs/guide/models.md) | LLM provider specs, thinking controls, bridges, embedders |
| [Command line](docs/guide/cli.md) | `serve`, `client`, `headless`, and scripting them |
| [What the agent knows](docs/guide/prompt-context.md) | AGENTS.md, briefings, skills and lessons |
| [Operations](docs/guide/operations.md) | multiple instances, access and tokens, backups, logs |

Design notes on how amplio works inside are in [docs/internals](docs/internals).

## Security caveats

Amplio does **not** sandbox the agent's shell: an agent runs commands as the
user that started the server, with that user's filesystem access, network access
and credentials. Run it somewhere you are comfortable with that.

Reads are also open to anyone who can reach the port — the token in the startup
URL gates *writes*, not reads. See [operations](docs/guide/operations.md) for
what that means and how to bind it down.

## Disclaimer

This is not an officially supported Google product. This project is not
eligible for the [Google Open Source Software Vulnerability Rewards
Program](https://bughunters.google.com/open-source-security).

## Citation

If you find Amplio helpful, please cite the following BibTeX:

```bibtex
@misc{Zhang2026Amplio,
  author       = {Chiyuan Zhang and Da Huang and Chen Liang and Andrew Li and {Amplio Contributors}},
  title        = {{Amplio: A Lightweight Agent Harness for Robust and Long-Horizon Runs}},
  year         = {2026},
  howpublished = {GitHub repository},
  url          = {https://github.com/google-deepmind/amplio}
}
```
