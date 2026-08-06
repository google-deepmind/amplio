---
name: run-manager
description: Driving and managing other amplio runs — submitting, watching, reading results.
scope: root
---

## Managing other runs

You can start and supervise other amplio runs with the `amplio` CLI, which is on
your PATH. Everything below talks to the server in the current data directory;
add `--data-dir=PATH` (or set `$AMPLIO_DATA_DIR`) to drive a different amplio
instance instead — a separate one keeps experiment runs out of the operator's
daily dashboard.

**Stream convention.** Every `client` subcommand prints the datum a script
consumes on stdout and everything addressed to a human on stderr. So
`RUN=$(amplio client submit …)` captures the run id and nothing else.

### Submitting

    RUN=$(amplio client submit --task=- --title="sweep A" --llm=MODEL < task.md)

Pass the task on stdin with `--task=-`, or as `--task=TEXT`. The
redirect is per-command, so this works inside a loop that is itself reading
stdin. `--llm` and `--title` are optional.

### Watching

    amplio client monitor $IDS [--interval=10m] [--timeout=12h]

Prints one tab-separated line per status change — `<run_id> <from> <to>` — and
exits when every watched run is finished at the same poll. Exit codes: 0 all
concluded, 2 any crashed, 3 any cancelled, 124 timeout, 1 an id that could not
be watched (unknown, or an interactive run). Statuses are sampled, so a change
that starts and ends inside one interval is not reported.

Do NOT poll in your own context: launch the watcher in the background and let it
wake you, so waiting costs no turns.

    ( amplio client monitor $IDS > /tmp/verdicts.tsv
      "$AMPLIO_NOTIFY" "batch finished (exit $?) — see /tmp/verdicts.tsv" ) &

Because it streams, you can also react per event — restarting a crashed run
brings it back under the same watch, since a run is only "finished" while every
run in the batch is:

    amplio client monitor $IDS | while IFS=$'\t' read -r run from to; do
      [ "$to" = crashed ] && ( sleep 3600; amplio client restart "$run" ) &
    done

### Collecting results

    amplio client list --json          # run summaries, including grades
    amplio client status $RUN --json   # one run in full

A run's final answer is its last assistant turn with no tool calls — the absence
of tool calls is what signals completion. Read it from the event log, and read
only the TAIL:

    eval "$(amplio client api /api/runs/$RUN |
            jq -r '.root_session_id as $s | @sh "SID=\($s) STEP=\(.sessions[]|select(.session_id==$s)|.current_step)"')"
    amplio client api "/api/runs/$RUN/sessions/$SID/events?from_step=$((STEP - 3))" \
          | jq -r '[.[] | select(.event.type=="assistant" and (.event.tool_calls|length)==0)] | last | .event.content'

`root_session_id` is the server's own answer to "which session is this run" — the
autonomous root, or the chatbot root for an interactive run. Do not hardcode
`main-agent`: a run may also carry a chatbot sidecar, and an interactive run has
no `main-agent` at all.

Use step range to avoid unnecessary traffic, and filter to find
the final assistant output w/o tool calls.

For "what happened" rather than "what was the answer", use
`/sessions/$SID/trajectory`: the phase titles and per-step summaries the observer
wrote, with no raw message text at all.

A sub-agent's result needs no fetch: it arrives in the parent's own log as a
`child_result` event carrying the text and a verdict.

The critic's report, one entry per iteration, oldest first:

    amplio client api /api/runs/$RUN/report | jq -r 'last | .summary'
    amplio client api /api/runs/$RUN/report | jq -r 'last | .key_achievements[]'
    amplio client api -X POST /api/runs/$RUN/report --timeout=10m   # (re)generate now

Artifacts: ask the server where they live rather than deriving the path from your
own $AMPLIO_ARTIFACT_DIR, which is the wrong answer the moment the run belongs to
a different amplio instance.

    amplio client api /api/runs/$RUN | jq -r .artifact_dir

`grades` on the run is the whole series, one per report iteration, oldest first,
which is what shows whether a run improved; `report_grade` is only the last one.

### Housekeeping

If housekeeping of the runs list is needed (confirm with the operator), use the
following utilities. One PATCH carries any of `archived`, `seen`, `starred`, `grade`, 
`title`, `note`:

    amplio client api -X PATCH /api/runs/$RUN --data '{"archived":true}'
    amplio client api -X PATCH /api/runs/$RUN --data '{"seen":true}'    # clear the badge
    amplio client api -X PATCH /api/runs/$RUN --data '{"seen":false}'   # flag for a human
    amplio client api -X PATCH /api/runs/$RUN --data '{"grade":"good"}' # or null to clear

Archive the noise you generated once its results are collected, and mark unread
the few runs a human should actually look at — the unread badge only appears
once a run has finished, so marking an ongoing run does nothing visible. Deleting (`-X DELETE
/api/runs/$RUN`) is irreversible — prefer archiving.
