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
propagated; the run's loops end at once. `restart` is the same path the server
takes at boot for crashed runs and is idempotent — a healthy run revives zero
sessions and prints `0`.

### loop

```bash
amplio client loop add <run-id> main-agent --every 30m "check the eval and report changes"
amplio client loop add <run-id> main-agent --every 2h --no-send-now --times 5 "summarize new failures"
amplio client loop add <run-id> main-agent --every 1d --verify-criteria "go test ./... passes" "run the integration tests"
amplio client loop edit <run-id> main-agent 1 --prompt "check the eval and the logs"
amplio client loop edit <run-id> main-agent 1 --no-verify
amplio client loop list <run-id>              # every session's active loops
amplio client loop list <run-id> main-agent --json
amplio client loop stop <run-id> main-agent 1
```

Sets a [loop](runs.md#loops): the prompt is sent to the session now and again
every interval. `add` prints the loop id on stdout. The flags are the loop
form's fields:

| flag | meaning |
|---|---|
| `--every D` | interval, at least `1m`, in whole minutes: `30m`, `90m`, `2h`, `1d` |
| `--no-send-now` | first tick after one interval instead of at once |
| `--ends D` | end this long from now (`7d`, `12h`; more than zero) |
| `--times N` | end after N ticks; with send-now, the first counts |
| `--never` | never end |
| `--rrule SPEC` | the whole schedule as iCalendar text, instead of all of the above |
| `--verify` | [verify each tick](runs.md#verifying-each-tick) with the server's `[verify]` defaults |
| `--verify-criteria T`, `--verify-model M`, `--verify-max-rounds N` | the request each tick's verification uses (each implies `--verify`) |

With none of `--ends`, `--times` and `--never`, the loop ends after the
server's `[loop] expiry` (7 days unless configured), which the client writes
into the rule. An `--rrule` spec is a `DTSTART` line followed by an `RRULE`
and any `RDATE`/`EXDATE` lines; separate them with newlines, spaces or a
literal `\n`. A `DTSTART` ending in `Z` is UTC; one without is the server's
local time. Times the command prints are the server's too.

The server accepts a subset of iCalendar whose evaluation is always quick:
`FREQ` MINUTELY to YEARLY with `INTERVAL` up to 100000; `COUNT` (1 to
1000000) or `UNTIL`, not both; `WKST`; and the filters `BYMONTH`, `BYMONTHDAY`,
`BYDAY`, `BYHOUR`, `BYMINUTE` and `BYSECOND=0`, each naming a value once
(`9` and `09` are the same value). MINUTELY and HOURLY rules take
no day filters (for "every 2 hours on weekdays", use DAILY with a `BYHOUR`
list), and must reach their `BYHOUR`/`BYMINUTE` values from `DTSTART` in steps
of `INTERVAL`. `BYDAY` ordinals (`1MO`, `-1FR`) are for MONTHLY and YEARLY and
can't be mixed with plain days. `DTSTART` may be at most a year in the past,
and it and any `RDATE` at most 100 years ahead:

```bash
amplio client loop add <run-id> main-agent \
  --rrule 'DTSTART:20261116T090000 RRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;BYHOUR=9;BYMINUTE=0' \
  "post the status update"
```

`edit` changes a loop from its next tick: `--prompt`, `--rrule` (a whole new
schedule; the tick count carries on), `--verify` and the `--verify-*` flags, or
`--no-verify`. Each `--verify-*` flag changes only its part of the loop's
request: `--verify-max-rounds 2` keeps the loop's criteria and model (a loop
that doesn't verify yet takes the server's defaults for the rest). Only one of
a session's loops can verify its ticks.

The same operations are on the API: `GET`/`POST
/api/runs/<run>/sessions/<session>/loops` with `{prompt, schedule: {kind:
"rrule", spec}, verify?: {criteria?, model?, max_rounds?}}`, and `PATCH`
(`{prompt?, schedule?, verify?}`; `verify` replaces the whole request, and
`"verify": null` stops verifying the ticks)
and `DELETE` on `.../loops/<id>`. A loop's `verify` reads back with the
defaults filled in. `GET /api/settings` reports the defaults a client fills in.

### verify

```bash
amplio client verify <run-id> <session>                          # on, with the [verify] defaults
amplio client verify <run-id> main-agent --criteria="go test ./... passes" --max-rounds=2
amplio client verify <run-id> <session> --off
```

Has an independent verifier check the session's next conclusion before it is
delivered (see [verified conclusions](runs.md#verified-conclusions)). Each run
starts a new version of the request (v1, v2, ...): a conclusion still with the
verifier for the old one is withdrawn, and the same verifier checks the next
one. Unset flags take
the server's `[verify]` defaults: `--criteria` (default: the session's current
request), `--model` (a spec or `#nickname`; default: `[verify] model`, else the
session's own model) and `--max-rounds` (default: the server's `[verify]
max_rounds`, 3 unless configured; `0` = unbounded). `--off`
turns it off, withdrawing a conclusion still with the verifier. The resulting
state (`on`, `verifying`,
`passed`, `not_verified`, `off`) goes to stdout. To read a session's
verification and its history: `amplio client api
/api/runs/<run-id>/sessions/<session>/verify`.

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

`--verify` on `run`, or on `resume` with `--message`, has an independent
verifier check the root agent's conclusion (to the task, or to the message)
before the root concludes; `--verify-criteria`, `--verify-model` and
`--verify-max-rounds` set the request and imply `--verify` (see [verified
conclusions](runs.md#verified-conclusions)). The run is at rest only once the
root really concludes, verified or not (a verifier that never answers can't
hold it forever: three stops in a row without a verdict, or one that can't be
started, conclude it not verified). The exit code keeps its meaning; the root's
outcome is logged at info level before `run at rest`: `verification passed`,
`not verified after N rejections`, or `not verified` with a `reason`. Verified
sub-agents log theirs at debug level only.

Once the run is at rest, the process writes the run report (a critic pass on
the HQ model) before it exits. `--no-report`, on `run` or `resume`, skips it
for one-shot runs whose report nobody reads.

## Managing runs with amplio

If your workflow requires starting, monitoring and managing a lot of runs,
you can use a meta run to manage them: start an interactive run by turning
on the `run-manager` briefing (see [prompt context](prompt-context.md)). The
chatbot can then use the `amplio` CLI to manage runs for you, either in the
same amplio instance, or a separate instance dedicated for sweeping runs.