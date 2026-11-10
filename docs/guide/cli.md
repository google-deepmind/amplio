# Command line

Amplio is one binary with three jobs: run the server, drive a running server, or
run a single task in the foreground with no server at all.

    amplio serve             the long-lived server: hosts runs, serves the UI
    amplio client …         talk to a server that is already running
    amplio headless …       run one task in this process and exit

Every command takes the global flags from [configuration](configuration.md) —
`--data-dir` above all, which picks *which* amplio you are talking to.

## amplio serve

```bash
amplio serve
```

The server owns the data directory: it holds the SQLite lock, runs the observer
that summarizes steps and phases, recovers runs interrupted by a previous exit,
and serves both the web UI and the API. Only one `serve` may own a data
directory at a time.

On startup it prints the URL to open:

    amplio → http://localhost:26759/?token=abcdEFGH123some-long-token

**That token is a credential.** Following the link authenticates the browser and
stores a cookie. Without the token or cookie, the UI shows a run read-only from
its URL, and the run list is private. The token is generated once and kept in
`<data-dir>/auth.token`, so it survives restarts.

Bind address: `--listen` > `$AMPLIO_LISTEN` > `config.toml`, defaulting to
`localhost:26759`. See [operations](operations.md) for more details.

## amplio client

`client` talks to the server that owns the current data directory, discovering
its address and token from `<data-dir>/server.json`.

```bash
amplio client list                            # this data dir's runs
amplio --data-dir=~/experiments client list   # a different amplio entirely
```

**Every subcommand prints the thing a script consumes on stdout, and everything
addressed to a human on stderr.** So `RUN=$(amplio client submit …)` captures a
run id and nothing else, while you still see the confirmation in a terminal.

### submit

```bash
amplio client submit "summarize the failures in ./logs"
amplio client submit --task=- < task.md
amplio client submit --task=- --title="sweep A" --llm=vertex-claude:claude-opus-4-8 < task.md
```

Hands a task to the server and returns immediately with the run id on stdout;
the run itself executes inside the server. The task comes from exactly one
source — a positional argument, `--task=TEXT`, or `--task=-` to read stdin —
and supplying two is an error rather than a silent preference.

Prefer `--task=-` for anything long. The redirect is also per-command, so it composes
inside a loop that is itself reading stdin:

```bash
while IFS=$'\t' read -r name spec; do
  amplio client submit --task=- --title="$name" --llm="$spec" < task.md
done < models.tsv
```

Other flags: `--workspace`, `--agent` (`standard_agent` or `chatbot`),
`--briefing NAME` (repeatable — see [prompt context](prompt-context.md)).

### list, status

```bash
amplio client list                 # table, most recent first
amplio client list --json          # same page as JSON, for scripts
amplio client list --archived --limit=200
amplio client status <run-id>      # one run: config, sessions, steps
amplio client status <run-id> --json
```

### monitor

```bash
amplio client monitor <run-id>...
```

Watches runs and prints one tab-separated line per status change —
`<run_id> <from> <to>` — exiting when every watched run is finished at the same
poll. A run that is already finished reports once with `-` as its previous
status; runs still working start silent.

| exit | meaning |
|---|---|
| 0 | all concluded |
| 2 | at least one crashed |
| 3 | at least one cancelled |
| 124 | `--timeout` reached (default 12h, max 50h) |
| 1 | an id could not be watched: unknown, or an interactive run |

Statuses are sampled every `--interval` (default 10m), so a change that starts
and ends within one interval is never reported. Interactive runs are rejected:
they park waiting for a human, so there is nothing to wait for.

Because it streams, you can react as things happen — and because "finished"
is evaluated over the whole set at each poll, restarting a crashed run brings it
back under the same watch:

```bash
amplio client monitor $IDS | while IFS=$'\t' read -r run from to; do
  [ "$to" = crashed ] && ( sleep 3600; amplio client restart "$run" ) &
done
```

### cancel, restart

```bash
amplio client cancel <run-id>    # cascades to every session under the run
amplio client restart <run-id>   # revive a crashed run's sessions
```

`cancel` returns once the request is accepted, not once cancellation has
propagated. `restart` is the same path the server takes at boot for crashed
runs and is idempotent — a healthy run revives zero sessions and prints `0`.

## amplio headless

```bash
amplio headless run --task="$(cat task.md)" --workspace=/path/to/repo
amplio headless resume <run-id>
```

Runs one task to completion in the foreground and exits, with no server and no
UI — the shape you want inside CI, an eval harness, or a container. This
process owns the data directory for its lifetime, so it **cannot** share one
with a running `serve`. To run several headless tasks on one machine, give each
its own data directory.

Agents' background jobs reach them with `amplio-notify` as they do under
`serve`, and so can a supervising script (`amplio notify --session=main-agent`
with `$AMPLIO_RUN_ID` set): the process serves just the notify endpoint, on
loopback with a per-process token, and records it in `server.json` while it
runs. Nothing else is served, so `amplio client` still needs a `serve`.

`resume` reconstructs the run's model, workspace and agent from its stored
config, so it needs no flags beyond the id.

`resume` revives only what was interrupted; a run already at rest stays
there. `--message=TEXT` first gives the root agent new input, which revives
even a concluded root: a harness's "time is up, answer now" turn. As with
`run`, the exit code says whether the command did its job, and the run's
outcome is in its state: if the message revives nothing (the root can't be
respawned; the log says why), `resume` exits 1, while a root that runs but
doesn't reply (it crashes, say) exits 0 with a warning, its status recorded.

When none of the run's agents is running any more, the process logs `run at
rest` with the `run_id` (a field of its own with `--log-format=json`), at info
level, so a harness that raises `--log-level` above info won't see it. All
that follows is exit work, the run report and the observer's last summaries,
which can take minutes, so a harness that only needs the outcome can stop the
process at that line. An interrupt (SIGINT) logs `interrupted, cancelling run`
instead. It stops the run's agents without changing their status, so a later
`resume` picks the run up where it stopped. In the second or so before this
`headless` process exits, nothing revives them: a notification that arrives
then is stored, and the agent sees it when the run is resumed.

Once the run is at rest, the process writes the run report (a critic pass on
the HQ model) before it exits. `--no-report`, on `run` or `resume`, skips it
for one-shot runs whose report nobody reads.

## Managing runs with amplio

If your workflow requires starting, monitoring and managing a lot of runs,
you can use a meta run to manage them: start an interactive run by turning
on the `run-manager` briefing (see [prompt context](prompt-context.md)). The
chatbot can then use the `amplio` CLI to manage runs for you, either in the
same amplio instance, or a separate instance dedicated for sweeping runs.