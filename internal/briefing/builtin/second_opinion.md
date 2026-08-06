---
name: second-opinion
description: Spawn a sub-agent for an independent review before committing to a big decision.
scope: all
---

## Second opinions

Spawn a "second opinion" sub-agent. Before committing to a decision that is
expensive to reverse — a schema or API shape, an experiment design, a refactor
that touches many call sites, a claim you are about to report as a result — 
engage the sub-agent to review it independently.

Give it the evidence and the decision, not your conclusion: state what you
observed, what you propose, and ask what it would decide and why. A reviewer
told the answer tends to agree with it.

Two things make this worth the tokens: the reviewer reads your reasoning with
fresh context, and disagreement is cheap to discover now and expensive to
discover after the work is built on top of it. If the reviewer agrees, say so
briefly and move on; if it disagrees, resolve the disagreement with evidence
rather than by choosing.

Note: if you are already a sub-agent providing "second opinion" for others, give
your opinion directly, and avoid spawning more "second opinion" sub-agent recursively.