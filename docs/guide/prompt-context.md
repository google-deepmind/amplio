# What the agent knows

Four different things end up in an agent's context, and they differ in who
chooses them and how long they last.

| | chosen by | applies to | delivered as |
|---|---|---|---|
| **system prompt** | amplio | every agent | the prompt itself |
| **briefings** | you, per run | that run's agents | appended to the system prompt |
| **AGENTS.md** | you, per machine or workspace | every run in that scope | a step-0 message |
| **skills / lessons** | the agent, on demand | one call | a tool result |

The system prompt — the agent's role, its tools, how a turn ends — is amplio's
and is not configurable. The other three are yours.

## AGENTS.md — standing instructions

Operator rules that should apply to everything, read from two optional files:

- `<data-dir>/AGENTS.md` — every run on this machine
- `<workspace>/AGENTS.md` — every run started against this workspace

If both exist they are concatenated, each under a heading naming its source
path, and seeded once at step 0 as a single message. Every agent in the run
sees it, sub-agents included.

Use it for things that are true regardless of the task: house style, a build
command that isn't obvious, a directory the agent should never touch.

**Read once, at run start.** The files are read when the run is created and the
combined text is stored on the run itself, so every agent in it receives that
snapshot — including sub-agents spawned hours later, which do NOT re-read the
file. Editing AGENTS.md therefore changes nothing for any run already going;
and would only impact new runs.

## Briefings — per-run context

A briefing is a named markdown file you attach to a run when you start it: an
API manual, a workflow, a piece of advice. They are common prompts that you
like to re-use, but not so common like AGENTS.md that applies to all runs --
you explicitly opt-in which briefings to enable when starting each run.

In addition to stock briefings, you can create your own briefings by adding
markdown files with front matter in `<data-dir>/briefings/`, e.g.:

```markdown
---
name: sweep-protocol
description: How we run optimizer sweeps here.
scope: all
---

## Sweeps

Change one axis at a time, and record the anchor run id in the log before
starting.
```

| field | meaning |
|---|---|
| `name` | the id — used to select it, and stored on runs that use it |
| `description` | one line, shown in the picker |
| `scope` | `all` (default) reaches sub-agents too; `root` only the run's main agent |

Sub-directories are for your own tidiness; identity comes from `name`, never
from the path, so moving a file never orphans the runs that reference it. A `/`
inside the name (`workflow/sweep-protocol`) groups the entry in the picker. A
file whose name matches a built-in overrides it.

The directory is scanned at startup, so a new file needs a server restart —
after which it appears in the picker alongside the built-in ones.

## Skills and lessons — pulled, not pushed

These two the agent fetches itself, when it judges them relevant, through
recall search.

- **Skills** are documents you supply: a directory per skill containing a
  `SKILL.md` with a `name` and `description`. Point amplio at your directories
  with `[skills] dirs` — see [configuration](configuration.md).
- **Lessons** are mined automatically. When a run's report is generated, the
  critic extracts reusable lessons from what happened and files them in the
  database, where later runs can find them.

Both need `embed_model` to be set; without it recall is disabled and amplio says
so at startup.

## Seeing what an agent was actually told

The composed prompt is written into the session's own log as its step-0 event.
You can inspect it in the trajectory viewer of the web UI or query via CLI:

```bash
amplio client api "/api/runs/$RUN/sessions/$SID/events?step=0" \
  | jq -r '.[] | select(.event.marker=="system_prompt") | .event.content'
```
