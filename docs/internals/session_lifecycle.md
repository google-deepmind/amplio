# Session Lifecycle

How an agent session is born, works, parks, ends, and is revived. Builds on the
step/turn model in [`step_model.md`](step_model.md).

A **session** is one agent's event stream plus a **status**. A **run** is a tree
of sessions (a root agent and the sub-agents it spawned). Sessions run as
in-process goroutines; a per-run registry maps `session_id` → a live handle
(notify + cancel). A session with no live goroutine is **cold** — it lives only
as DB rows until something revives it.

## Status (logical)

Status is the session's *logical* state. It does **not** encode whether a
goroutine is currently alive — that's a physical optimization (see
[Hot vs cold](#hot-vs-cold-physical-only)). Every non-`ongoing` status is
restartable; *how* is defined by the [matrix](#what-advances-a-session).

| Status | Meaning |
| --- | --- |
| `ongoing` | actively working in the loop |
| `awaiting` | parked via `await_event`, in a `conclude` call waiting for its verifier's verdict, or by the harness waiting for the verdict on a reply's candidate — actively anticipating new events |
| `idle` | parked after a bare no-tool-call turn (interactive agents only) |
| `concluded` | finished — delivered its result (autonomous agents only) |
| `crashed` | stopped by an uncaught error, durably recorded |
| `cancelled` | explicitly stopped by an operator or parent |
Normal "parked" states:

- An **interactive agent** (chatbot) parks as **`idle`** (turn done, waiting for
  the user) or **`awaiting`** (a background task is running, standing by). A
  conversation is never `concluded`.
- An **autonomous agent** ends as **`concluded`** (with the result it gives the
  `conclude` tool) or parks as **`awaiting`** (waiting on sub-agents, or inside
  a `conclude` call for its verifier's verdict). It never enters `idle`.

A no-tool-call turn means "I have nothing more to do right now," and what it
produces depends on the agent's nature (see
[Termination](#termination-conclude)): an interactive agent becomes `idle`; for
an autonomous agent it is an attempt to conclude with its text, as a
`conclude` call would be.

### Two notions of "crashed"

- **`crashed` status** — durably recorded, which (via the atomic
  [child-result](#child-results-exactly-once) write) means the parent was already
  notified. A crashed *sub-agent* is never revived spontaneously — only by an
  `Input` follow-up (e.g. a parent retry). A crashed *root* is revived by
  [Recover](#recover) on restart.
- **`ongoing`-but-dead** — status is still `ongoing` but no goroutine exists (a
  hard process crash before any transition was recorded). [Recover](#recover)
  revives these; since nothing was recorded or notified, re-attempting is correct.

## Hot vs cold (physical only)

After a no-tool-call **park** (`idle`), or while `awaiting`, the goroutine lingers
for an idle timeout (default 30 min) as an optimization. (A `concluded` autonomous
agent does **not** linger — its goroutine exits immediately; a later `Input`
revives it cold.)

- **Hot** — goroutine still alive/registered; revival is an instant in-memory
  wake with context intact.
- **Cold** — goroutine exited; revival respawns from DB state.

**Hot vs cold changes only the *mechanism* (wake vs respawn), never the
*outcome*.** The same event produces the same logical result whether the session
is hot or cold; the 30-minute window is purely a latency optimization. (The one
operation genuinely *about* physical liveness is [Recover](#recover), which
respawns logically-active-but-physically-dead sessions.)

## What advances a session

A session is moved by one of **two stream-event classes** (events materialized in
its stream) or the **Cancel** action. The run-level [Recover](#recover) operation
is separate — it merely *produces* events of the `Input` class.

`Classify(evt)` sorts every stream event into one of two classes by whether it
should revive a *dormant* session:

| Class | Revives a dormant session? | Examples |
|---|---|---|
| **Notice** | no — persists; only a live `awaiting` session reacts | `ChildResultEvent` verdict `crashed`/`cancelled`; a verifier's outcome (`VerifyEvent` `verdict`, below); self/system writes (Assistant, ToolResult, Compaction, `SystemEvent` markers); `UnknownEvent` (below) |
| **Input** | yes — restarts any non-`ongoing` session (but see the environment-notification exception below) | `UserEvent` (operator/user input, including a loop's tick); `MessageEvent` — agent (`send_message`) **or** environment (`$AMPLIO_NOTIFY`); `ChildResultEvent` verdict `concluded`; `RecoverEvent` |

**Record-only events wake no one,** not even a live `awaiting` session
(`db.IsRecordOnly`): a `ToolResultEvent` (the session's own turn output), a
`VerifyEvent` (a change to its verification; not a verifier's `verdict` nor
the harness's `withdrawn`, which end a wait: see Verification), a `LoopEvent`
(a loop set, changed or ended: that shouldn't cost an agent a turn or end a
wait it was in for a job; the loop's ticks are what wake it), and an
`UnknownEvent` (below).
The session reads them at its next turn, whatever starts it (but for a
`LoopEvent` or an `UnknownEvent`, which it isn't shown); `await_event`, a
`conclude` call's wait for a verdict and the "arrived during generation" check
don't count them.

**Every event is written to the session's stream regardless of the target's
state** — the class only decides whether it *revives* a dormant session. A
`Notice` to a terminal session just persists (seen only if the session is later
restarted).

**Events from a newer binary.** After a downgrade (a rollback, or an older
pinned binary on the same data dir), a session can hold an event type this
binary doesn't know. The store reads it as an `UnknownEvent` placeholder rather
than failing the read. It stays in the stream, and the trajectory shows it as
stored, labelled as a newer amplio's, but nothing else uses it: not the model's
context, the chat, the inspect and search tools, the summaries, or the checks
on a stream's tail (a final answer followed by one is still at rest). It is a
`Notice` and record-only: it wakes nothing. This covers new event *types*. A
newer binary can also add a value or a field to a type this one knows, which
is up to that type's decoder: unknown fields are dropped on read, and a type
whose behaviour turns on a value (an action, say) should read a value it
doesn't know as the placeholder too, rather than misread it. A `VerifyEvent`
or a `LoopEvent` with an action this binary doesn't know does. State mirrored
in session metadata keeps what it doesn't know: a loop in the `loops` mirror
keeps the fields a newer binary added, so this one's rewrites don't drop them.

**Environment notifications (`$AMPLIO_NOTIFY`) are `Input`-class, but do NOT
revive a *terminal* session.** They revive a *parked* recipient (`idle`/
`awaiting`), so a background job's completion notice reaches an agent that's
merely waiting even if it forgot to `await_event`. They do **not** revive a
`concluded`/`crashed`/`cancelled` session: the environment must not resurrect an
agent that has deliberately finished (or was deliberately stopped), and reviving
a terminal just to trivially ack a stale/runaway notifier churns phases, run
reports, and — via a re-posted `child_result` — the parent chatbot. A terminal
session is still restartable by a `UserEvent`, an agent `send_message` /
`child_result`, or `Recover` — only a raw env notification is gated. This
exception is enforced at the wake path (`runtime.NewCommitNotifier`, against a
wake-path-private `envUnrevivableStatuses` set — deliberately NOT a general
"terminal" predicate, since that notion is overloaded and this policy needs to be
tunable on its own, e.g. `crashed`), not in `Classify` (which stays a pure
function of the event):
the env message is still appended (persisted) to the terminal session's stream,
it just doesn't respawn it, so it's seen on the next genuine restart. A
runaway/forgotten notifier remains recoverable: `notify` stamps its process
group onto the sender (e.g. `environment (pgid=12345)`), so a revived agent can
stop the offending script and everything it started with `kill -- -12345`. Not
the parent pid: once the shell that launched a backgrounded notifier exits, its
parent is whoever adopted it — init, or `systemd --user` under a systemd unit.

**Env notifications are capped per step** (`server.maxEnvNoticesPerStep`, 50).
Since they persist even when they cannot wake anyone, an abandoned poller would
otherwise append for as long as it runs — and every notice it appends is also
replayed into the model's context on the next turn. The *step* is the budget
window, which needs no session state to be the right one: a working session
advances its step every turn, so 50 is a generous per-turn allowance; a finished
session never advances again, so the same rule becomes a lifetime cap. Over the
cap, `POST /notify` answers **429** instead of appending, with the fixed token
`env_notice_capped` as the body (a script, not a model, is reading it — so no
prose and no live count, which would defeat an exact match). Since `amplio
notify` maps non-2xx to exit code 3 with the body on stderr, a stray loop can
detect this precisely and stop itself.

Relatedly, since a terminal session can no longer be env-rescued, an autonomous
agent that ends a turn with **no tool calls AND empty content** (a likely
*accidental* conclusion — the model reasoned but forgot to act) is nudged back
into the loop rather than concluding on nothing, bounded by a small in-memory
counter so a degenerate model can't loop forever (see
`eventloop.maxConcludeNudges`). If it still produces nothing, the session
crashes (`errEmptyResult`) instead of handing its parent an empty "successful"
result.

A turn the provider **refused** (`AssistantEvent.Refusal` is set: an Anthropic
`refusal` stop, a Gemini blocked prompt or policy stop) is not an accidental
conclusion and is not nudged: replaying the same context would usually be
refused again. An autonomous agent instead ends **`crashed`** with the refusal's
stop reason, category and explanation as the error, whether the refused turn
carried no text or a partial reply — so the parent gets
`child_result(crashed, <reason>)` rather than an empty `concluded` result. The
same check runs on the resume path when a refused turn is the stream tail. An
interactive agent just parks `idle`; the operator sees the refusal in the chat.

**Loop ticks are operator input.** A loop (`internal/loop`) re-sends the
operator's prompt to a session on a schedule. Each tick is a `UserEvent` with
`Loop: {ID, N}` and the content `[loop <id> · <schedule> · tick <n>] <prompt>`
(with `· verification v<k>` before the `]` for a tick that started a version),
appended like a typed message and delivered through the same commit-notifier
path: a busy session reads it at its next step (a turn longer than the interval
collects several ticks and sees them together), and an `idle`, `awaiting`,
`concluded` or `crashed` one restarts. That is the point of a loop and the
difference from `amplio-notify`, which can't revive a finished session: a loop
is the operator's standing instruction, so it may. A loop that verifies each
tick (`loop.Loop.Verify`) starts a new version of the session's verification
with each tick, through `verify.Tick` (`VerifyEvent.By` = `loop <id>`, on the
operator's behalf): the set or replace and the tick (`UserEvent.Loop.Version`
names the version) are one write (`Store.AppendEventsIf`, with the tick's
checks below), made under the verification lock, and only then is a candidate
the new version supersedes withdrawn. So whatever wakes the session (the tick, or the withdrawal ending a
`conclude` wait) finds the version and its tick together. A loop that stopped
verifying turns off its own version, if still in force, in its next tick's
write. A tick that couldn't be written started nothing; it is retried, backing
off (1, 2, 4 … minutes, up to an hour), and once the schedule has no occurrence
left and the tick has been due for an hour, the loop ends without it. A session
has one verification, so only one of its loops may verify its ticks.

**A cancel ends loops.** Cancelling is a deliberate stop (by the operator, a
parent's `session_cancel`, a concluding parent's cascade, or a run cancel), so
no tick may undo it, directly or through a sub-agent: a finished sub-agent
revived by a tick would post its result to the cancelled parent and revive
that. So cancelling a session ends its loops and those of every session under
it, whatever their status ("session cancelled", or "session <id> cancelled"
under a cancelled ancestor). The store's session-status listener, which every
cancel fires through `TerminateAndNotifyParent`, does this at once. A tick that
comes due checks the session and its ancestors again, as does startup, for a
cancel this process didn't see. Cancelling a run ends all its loops directly
("run cancelled"), since the cascade leaves an already-finished session alone
and changes no status there. A loop can't be set on a session that is, or is
under, a cancelled one. A crash is not a deliberate stop: the next tick revives
a crashed session.

A cancel can land while a tick is on its way (past those checks, waiting for
its write, or for the verification lock if it starts a version). So the tick's
write re-reads the session and its ancestors in its own transaction
(`AppendEventsIf`) and writes nothing under a cancelled one, version included;
and ending a run's loops (archive, run cancel) marks them first, so that a tick
on its way doesn't land either and revive a concluded root that the run
cancel, which follows, then leaves alone.

The loop's own bookkeeping is a `LoopEvent{Action: set | edit | stop | ended}`
in the stream: record-only (above), and not shown to the agent, which learns of
a loop from its ticks. The inspection tools (`session_steps`, `session_summary`
and the rest) and compaction show it without the prompt, which would read as a
request, and with the schedule in words (`Summary`: the `Rule.Describe` the
ticks' labels use, recorded by a set or an edit).

The scheduler lives in `serve` only. It keeps each session's active loops in
session metadata (`loops`), so startup re-arms them without reading streams.
Whatever a schedule says:
- **A tick comes at least a minute after the occurrence the previous one was
  sent for.** Occurrences closer than that, and several that come due together
  (after a late timer, a failed write's retry, or a restart), become one tick
  for the latest; the others are spent. A tick that went out late doesn't push
  the later ones off the schedule; by the wall clock the gap after it is at
  least 55 s.
- **Missed ticks aren't replayed.** Occurrences that passed while no server ran
  are spent, and count toward an `RRULE` `COUNT`.
- **A loop ends** when its schedule runs out ("schedule finished after N
  ticks"), on a cancel (above), and when its run is archived ("run archived")
  or deleted (dropped). It also ends when the tick this process last sent to a
  session at rest (`concluded`, `crashed`, `idle`) never got it running, the
  session still at rest at that step, when the next comes due: reviving it
  failed (an unknown agent type, an invalid workspace: `RespawnSession` logs
  why), and ticks would only pile up unread. The scheduler hears the session go
  `ongoing` through the store's session-status listener. A session that did
  run keeps its loop, though it is at rest again at the same step: the turn a
  tick revived it for can crash as the one before did (a model still down)
  without reaching its next step.
- **Walks stay short.** rrule-go evaluates a rule from its `DTSTART`, so a
  tick of a loop whose `DTSTART` is more than 30 days old moves it up to the
  occurrence the tick is for, when that leaves later occurrences unchanged (no
  `COUNT`, which counts from `DTSTART`; and not onto an occurrence in the hour
  clocks spring forward over, which rrule-go gives an hour early, and which as
  a `DTSTART` would move every later one). `COUNT` is at most 1000000, and a
  session has at most 20 loops, whose metadata mirror every change rewrites.
- **A backward step of the wall clock pauses loops** for its size, rather than
  repeating ticks they already sent.

Each session's loops change under that session's write lock, held across the
change's DB write and the wake path it runs, so a slow delivery holds up only
its own session. Headless runs don't arm loops, so a loop set under `serve`
doesn't fire during `headless resume`. (If they ever do: once a headless
process is interrupted, its registries stay closed until it exits, so no input,
a tick included, can revive a session there.)

**Operator/user input is a `UserEvent`, not a `MessageEvent`.** `MessageEvent` is
agent-to-agent (`send_message`) or environment (`$AMPLIO_NOTIFY`); rendering user
input as a `MessageEvent` would tempt a chatbot to "reply" to the user via
`send_message`. The web/CLI persists typed input directly as a `UserEvent` (a plain
user turn).

The matrix — "restart" = wake (hot) or respawn (cold); same outcome:

| State | Input | Notice | Cancel |
|---|---|---|---|
| `ongoing`† | queue; the loop reads it | queue; the loop reads it | stop + mark cancelled + cascade |
| `awaiting` | wake → process | **wake → process** | stop + mark cancelled + cascade |
| `idle` | restart → process | persist | stop + mark cancelled + cascade |
| `concluded` | restart → process | persist | NOP (terminal) |
| `crashed` | restart → process | persist | NOP (terminal) |
| `cancelled` | restart → process | persist | NOP (terminal) |

† `ongoing` means *alive and working*. An `ongoing`-but-dead session is a
crashed-process case revived by [Recover](#recover) (or the next `Input`).

Reading the matrix:

- **Every non-`ongoing` state restarts on an `Input`** — including `cancelled`.
  You bring a session back by **sending it a message** (see
  [Restart vs Recover](#restart-vs-recover)).
- **`awaiting` is the only state where a `Notice` wakes it.** `await_event` opted
  into "any event," so an awaiting parent *does* wake on a child crash (it wants
  to know). Every other parked state ignores `Notice` — if you're anticipating an
  event, `await` it.
- **A `Notice` never revives a dormant session.** This is what stops a crashing
  child from hammering a dormant parent back to life: the report persists and is
  seen on the next genuine restart.
- **`concluded`, `crashed`, `cancelled` are identical here** — all terminal, all
  restartable by `Input`. They differ only in their **verdict** label and in
  whether [Recover](#recover) touches them (only a `crashed` root). `idle` is the
  one non-terminal parked state, hence the only one Cancel acts on.

## Restart vs Recover

Two different ways a session comes back — don't conflate them:

- **Restart** (session-level, reactive): a non-`ongoing` session revives because
  an **`Input`** arrived for it — typically a follow-up **message**. There is no
  "resume this session" button; you restart a session by messaging it. Any
  non-`ongoing` session (`idle`/`concluded`/`crashed`/`cancelled`) can be
  restarted this way.
- **Recover** (run/system-level, proactive): on a process restart, or when an
  operator resumes a **run**, the system re-spawns the run's *active spine*. It is
  never aimed at a single dormant session. See [Recover](#recover).

## Cancel

Cancellation is **canceller-driven, not cooperative**: whoever cancels — the
`session_cancel` tool, an operator, or a concluding parent cascading to its
children — does all the work; the target agent just stops.

A single `cancelSession(sid, reason)` primitive, applied recursively:

1. if already terminal (`concluded`/`crashed`/`cancelled`) → **NOP**;
2. in **one atomic write** (`TerminateAndNotifyParent`, carrying `reason`): set
   status `cancelled`, write a `cancelled` `SystemEvent` marker to the session's own
   stream, and notify the parent. The terminal write fires the session-handler for
   the **parent only** (the `child_result`), never for the target itself — so it
   never self-wakes the session being stopped;
3. recurse into each active child (with a reason such as `"parent <id> concluded"`);
4. if a goroutine is live, **ctx-cancel** it so it stops.

The agent loop contains **no** cancellation logic — it treats a cancelled `ctx`
(parked in `await`, mid-LLM-call, or between iterations) as "stop now, status
already set, return." A parked goroutine's wait is simply interrupted; nothing is
ever "woken so it can cancel itself."

**Reasons make cancellation observable and recoverable.** The `reason` flows into
the parent's `child_result(cancelled, …)` and onto the cancelled session's own
stream. So when a parent concludes while sub-agents are still running, they are
cancelled with `"parent concluded"`, and the parent's stream records exactly what
was cancelled and why. If that was a *mistake*, an operator restarts the parent
(a follow-up message); the parent sees the cancelled-children records and can
**re-spawn** them.

Tradeoffs (accepted): ctx-cancel abandons in-flight work immediately — fine for
cancel, since the work is being discarded (tools must keep writes atomic /
ctx-safe). A cancel landing exactly as a session concludes is a benign race (both
mean "ended"); cancelling mid-`spawn` can briefly miss a just-created grandchild
([Recover](#recover) catches the orphan). And the recovery above **re-spawns**
children (fresh) — a cancelled child's progress is lost. Progress-preserving
revival would need a future agent-callable `resume_child` (a parent sending a
`RecoverEvent` to its own child); not built.

## Recover

`Recover` is the narrow, run/system-level crash-recovery mechanism — it has
nothing to do with restarting an individual dormant session (that's an `Input`).
It runs when the process restarts, or when an operator resumes a **run** (the
`amplio headless resume <run-id>` command), and it re-spawns only the run's **active
spine**:

- `ongoing`-but-dead sessions (were working),
- `awaiting`-but-dead sessions (were waiting; re-arm them), and
- a `crashed` **root** (no parent to retry it).

It does **not** touch `idle`/`concluded`/`cancelled`/`crashed`-sub-agent — those
are settled or dormant and come back only via a reactive `Input`.

Mechanically, Recover appends a **`RecoverEvent`** to each spine session.
`RecoverEvent` is an `Input`-class event, so it flows through the normal respawn
machinery (re-enter the loop with status `ongoing`) and doubles as the visible
"resumed" marker the LLM sees. It is **only** produced by Recover — never by
normal agent flow. Its content also says what the session still has running:
its live background processes (from the process table, attributed by the
`AMPLIO_SESSION_ID` in each process's environment; each with its process group,
age and command) and its running sub-agents, or that nothing survived. Without
it an agent learns only that its wait was interrupted, not whether the job it
was waiting for is gone. (Subtlety from crash recovery: Recover **skips** the
`RecoverEvent` when the stream is already "at rest" — crashed after the final
reply but before the status flip — to avoid a redundant LLM round-trip, and
for a session whose `conclude` call is still waiting for its result, whose
resumed wait it would cut short; see [Crash Recovery](#crash-recovery).)

## Termination: conclude

An autonomous agent ends its work by calling the **`conclude` tool** with its
result; an interactive one by a turn with **no tool calls**:

- **Autonomous agent → `concluded`.** `conclude(result)` delivers `result`: it is
  posted to the parent (the return value) and the agent cascade-cancels its owned
  children (with a `"parent concluded"` [reason](#cancel)). The call is
  exclusive (alone in its turn, so what it concludes with can't depend on
  results still to come), and every call gets a tool result: the one that ends
  the session is written in the same transaction as the conclusion (a
  `TerminateAndNotifyParent` self event at the call's step), so a session
  revived later sends the provider a history in which every tool call has its
  result. A conclusion check may hold the call instead (below); its answer is
  the call's result, marked with the check (`ToolResultEvent.Check`). Input
  that arrived while its turn was generated (other than its verifier's
  outcomes) comes first, as for a reply: the call returns saying what came
  before any check runs, so nothing it offers is delivered or given to a
  verifier, and the agent calls it again once it has read it. The same holds
  for input that arrived while the checks ran, up to the moment a candidate
  would go to the verifier: that is checked again under the verification
  lock, under which a new version's message (a loop's tick, a message with
  verify) is written, so a result never becomes the candidate of a version
  whose request it didn't read. Nor does a call conclude over input that
  arrived while it waited (other than its verifier's outcomes, which it
  settles): it returns saying what came.
  A wait's status writes never overwrite a cancel that landed meanwhile
  (`SetStatusUnlessTerminal`, as in `await_event`), and a conclusion whose
  terminal write finds the session already stopped delivers, and logs,
  nothing.
- **Interactive agent (chatbot) → `idle`.** It has no `conclude` tool; a turn
  without tool calls rests it until the next user message, or any background
  sub-agent's result.

Model it as a function: `spawn` = async call, `await_event` = await,
`conclude(result)` = `return result`, `child_result` = the returned value.

**A reply without tool calls** from an autonomous agent (one that answers
instead of calling the tool, or a type without it) is taken as
`conclude(result=text)` when no conclusion of its own is with its verifier
(below): with nothing holding it, it concludes with no extra turn. While a
candidate is pending, a reply is never a candidate: it gets a nudge
(`UserEvent.Check = "conclude"`) to call `conclude` (or `await_event`), and
after two nudges in a row the next reply is taken as `conclude()`, a wait for
the pending candidate, so a model that never calls the tool neither replaces
its candidate nor spins. (A message about its verification starts a new run of
nudges.) A reply has no call to wait in, so where a call would wait for a
verdict the harness waits instead, after a `Check: verify` note saying so, and
writes the outcome as a message. Wherever a check holds a reply rather than a
call, its answer is a user-role message marked with the check.

With `[agent] require_conclude` on (`Env.RequireConclude`; off by default), a
reply never concludes an agent that has the `conclude` tool and concludes with
a result (`eventloop/replies.go`): it gets a note (`UserEvent.Check =
"reply"`) asking for `conclude`, and the fifth reply in a row, counted as notes
since the agent's latest turn with tool calls, fails the session (crashed,
"kept replying without calling conclude"); the fourth note says so. A reply a
crash left at the end of the stream gets its note on restart rather than
concluding. Empty and output-limited replies keep their own nudges, and a
verifier's replies their rules (never a verdict).

**End-of-run reminder.** Concluding is final: running sub-agents are cancelled,
and notifications from the agent's background processes reach no one afterwards
(see the environment-notification exception above). So an autonomous agent about
to conclude while its own background processes (attributed by
`AMPLIO_SESSION_ID`, as the Processes tab does) or sub-agents are still running
gets one reminder instead (as its `conclude` call's result, or a `[system]`
message after a reply), listing them and what concluding will do, and decides
whether to wait, stop them, or conclude anyway. The attempt right after a
reminder always concludes; an agent that goes back to work and concludes later
is reminded again if anything is still running. The check sits in front of
every place that concludes an agent (a `conclude` call, a reply, and the
recovery path for either), and the only state is the reminder in the event
stream. Interactive agents never reach it: they go idle, and notifications wake
them. What a concluding sub-agent leaves running is also reported on its
parent's `child_result`, as a note (`notes`: kind `leftover`, e.g. "1
background process group"; events written before notes existed carry it as
`leftover`), outside the result text. The nudges for an empty or
output-limited final turn are user-role messages marked with their checks
too, so UIs show them as amplio's rather than the operator's.

**Verification** (`internal/verify`; the check is
`internal/agent/eventloop/verify.go`). The second conclusion check, after the
reminder. An operator or a parent can ask that an autonomous session's next
conclusion pass an independent verifier first; the request and everything that
follows is a `VerifyEvent` in the session's stream (`set`, `replace`, `clear` by
the requester; `verifying`, `rejected`, `passed`, `exhausted`, `withdrawn`,
`stale` by the harness; `verdict` by the verifier's own terminal write, below),
which is the whole state. Reading it (`verify.Load`) takes only those records
and the user messages and sub-agent results that tell of the verifier's
outcomes, through the index on the event type (`EventFilter.Types`), not the
whole stream; a stream with a `verifying` record from before records carried
their candidate is read whole. A `VerifyEvent` is record-only, but for a `verdict`
and a `withdrawn`: setting verification on an agent parked in `await_event`
doesn't wake it, nor does it revive a dormant one. What must wake an agent
comes with an event of its own: its verifier's outcome, and the `withdrawn`
record a new version or a clear writes when it withdraws a candidate the
verifier has, which ends a wait for that candidate (a Notice: it revives no
one). With a message, the `VerifyEvent` (and any `withdrawn`) is written
first and the message after it, still under the verification lock
(`verify.SetThen`): a `conclude` call the withdrawal wakes settles under that
lock, so its next turn reads the message too, and the message wakes an agent
that wasn't waiting with the change in place. The agent reads someone else's
change as a note in its context, with the version it starts or ends; but a
version a loop's tick starts is named by the tick's label, and its note is one
line of the loop's criteria, or nothing if it has none.

**Versions.** Every `set` or `replace` starts a new version of the
verification (v1, v2, …), counted from the stream (`verify.State.Version`, read
from the whole stream, so a compaction doesn't renumber them); the candidates
given to the verifier in a version are numbered too (`Candidates`; a
re-engagement with the same candidate after a stop isn't a new one). The
labels ("v2, candidate 1") are in what the agent and the verifier read, in
`GET .../verify` and in the Inspector. Every version after a session's first
goes through `verify.Set`, which returns the new version's number (the first
can also be written when the session starts). A new version or a clear
withdraws the candidate the verifier has (`withdraw`): the `withdrawn` record,
whose reason names the candidate and what withdrew it; a verdict the verifier
gave on it that the agent hadn't acted on, recorded `stale` right there; and,
for a verifier still at work on it, a harness message (`Check: withdrawn`):
"v2 is withdrawn; stop and wait for the next candidate". The verifier isn't
cancelled, so it keeps what it learned; one that is done gets no message,
which would only revive it (the read and the message aren't one write with
the verifier's own conclusion: one that concludes in between is revived by
the message for a turn in which it stands by, and its verdict, late, is
recorded stale). The verifier is retired (cancelled; the next conclusion
starts a new one) by a version that asks for another model than the
verifier's request had, and after a version it ended itself (it stopped too
often, or couldn't start), with the same bookkeeping. A clear keeps it, idle,
until a later version or the agent's conclusion cascades to it.

A verifier whose latest candidate was withdrawn (a `Check: withdrawn` message
after it in the context its turn answered) stands by: whatever it ends its
turn with, a reply or a `conclude` call with or without a verdict, concludes it
quietly, with no outcome for the agent (the terminal write has no parent to
notify), and its stops while it stands by don't count. A verdict call held
because the withdrawal arrived while it was generated is told so ("your
verdict was on a withdrawn candidate ... end your turn without a verdict"),
never invited to deliver the verdict again. The next candidate re-engages it
like any other; the first candidate of a version it hasn't seen the request of
(`VerifierSince`) brings the version's request and criteria.
A verdict that lands after the version changed, before a candidate of the new
version went out (`verify.State.Late`), was on a withdrawn candidate: the
agent's next read of its verifier under the verification lock (the note at its
next turn, a settle, an attempt, a conclusion) records it `stale`, with the
verdict and why, once, and the note tells it as information. A stale verdict
changes nothing: no round, no pass.

A verification's outcome that concludes the agent (a pass, or the end of the
verification) is decided under the verification lock, and the lock is taken
again and held through the terminal write: a new version or a clear in
between withdraws the candidate (the call is answered as superseded) rather
than letting the old version's outcome deliver it or end it.

**A verifier's outcome reaches the agent as the harness tells it, not as a
`child_result`.** A verifier is marked with session metadata `verifier_for:
<agent>`; when it reaches a terminal status, `TerminateAndNotifyParent` posts
to the agent, in the same transaction and in place of a `child_result`, a
`VerifyEvent` `verdict` carrying its outcome (`event.VerifyOutcome`): `PASS` or
`FAIL` with its findings, or, for a crash or cancel, its status and why, and
the transition's time (`At`, which identifies the outcome). It is a Notice: it
wakes the agent waiting for it (it isn't record-only), but revives no dormant
one, with one exception the wake path makes (`runtime.NewCommitNotifier`): a
verdict revives a **crashed** agent whose pending candidate it judges, as a
sub-agent's result revives a crashed parent, and its revived turn reads the
verdict. Nothing else revives: an agent with nothing pending (concluded, with
its verifier revived and concluding again) never takes a turn with nothing to
read. It isn't shown to the model: the agent reads what the
harness makes of it (below), a structured outcome on a `conclude` call's
result or on a verify message (`Outcome`), so the verifier's output stays in
its own session and a verdict for an earlier candidate is never told as if it
were for the latest. Every path that ends a verifier (its conclusion, a
crash, a cancel, a panic in any of its lives, recorded as a crash) goes
through that one write.

While verification is on, `conclude(result)` makes `result` a candidate and
gives it to a verifier: a sub-agent of the agent of the `verifier` agent type
(`internal/agent/verifier`), started by the harness through the same path as
`spawn_agent`, or the existing one re-engaged with a `Check: verify` user
message (an Input, so it revives a stopped verifier). Then the call waits,
as `awaiting` and with no timeout, until something that wakes arrives, and
settles what the verifier said: a pass concludes the session with exactly that
candidate; a rejection counts a round and returns the findings, and at the
limit concludes with the candidate, not verified; a verifier that stopped
without a verdict is not a round (the call returns, saying why it stopped, and
`conclude()` re-engages it), but `verify.MaxStops` (3) such stops in a row
conclude it, not verified;
a new version or a clear withdraws the candidate (its `withdrawn` record ends
the wait; the call returns "superseded by v3; your conclusion for v2 was
withdrawn", and the next `conclude(result)` goes to the new version, or
through). Anything else that
arrives ends the wait early: the call returns saying what came, and
`conclude()` without a result waits again (or settles at once if the verdict is
in). `conclude()` with nothing pending is an error. Every attempt passes the
end-of-run reminder first, `conclude()` included: work started after an early
return is reminded of before a pass could conclude over it.

The candidate is what the harness gave the verifier, carried by the
`verifying` record (`candidate`): a reply taken as `conclude()` gives the
pending candidate again, never its own text. A verdict counts only for the
latest candidate of the current version (the verifier concluded after
receiving it, matched by the order of the verifier's own stream, not by
anything the verifier says: a typed verdict as given; a verifier type's reply
is no verdict; an older standard-agent verifier's result by its first line).
One that arrives while the agent works or waits in its own
`await_event` (a `conclude` call is the only wait that settles verdicts) wakes
it, and is told at its next turn in a `Check: verify` message written where the
outcome landed: a pass ("call `conclude()` to deliver it, marked verified, or
`conclude(result)` to submit a new one"), a rejection with its findings (whose
round it counts), the end of the verification, or a stop with why. Once the
end is told, the next conclusion concludes, not verified, whether it delivers
the last candidate (`conclude()`) or a new result: the last rejection, which
only the conclusion records, still counts (it was told, from the stream after a
restart too). A rejection's `rejected` record is written with the answer that
tells it, call result or message, in one transaction (`Store.AppendEvents`), so a counted
round is never left untold; a `rejected` that isn't of a pending candidate (one
that raced a replace or clear) counts nothing. Each outcome is told once: what
tells it carries it, with its `At`, and nothing tells it again, neither the
next `conclude` call nor a later message (an early return that told no verdict
doesn't count).

A verdict that isn't on the candidate pending now is told as information, not
as a round (`VerifyOutcome.Earlier`, shown as such in the chat; a message while
the agent works says to call `conclude()` to wait for the pending one's
verdict): a rejection that lands while the agent submits a newer candidate,
before it was told (the attempt counts no round; its findings come in a note
before the wait), or
the verifier's verdict on an earlier candidate it concluded on as the latest
reached it; one on a candidate a new version or a clear withdrew is recorded
stale and told as such. A pass marks one candidate verified; it doesn't switch
verification off for a new result, which goes back to the verifier. Repeating
the candidate the verifier has (`conclude()`, or the same result) doesn't
message it again.

A verifier whose conclusion a new candidate overtakes (it arrived while the
verifier generated its verdict) is told that verdict was on the earlier
candidate and doesn't count, and to check the new one, never to deliver it
again (a message giving the same candidate again, after a stop, is no new
one). Giving the same verdict again straight away gets the same answer,
twice, after which the verifier stops without a verdict, as a crash does.

**The verifier agent type** is the standard agent's tools with a prompt of its
own (the role, what counts as evidence, and that it shares the agent's
workspace and must leave it as it found it) and a typed `conclude(verdict:
PASS | FAIL, findings)` instead of a result (`agent.Traits.Verifier`). A FAIL
needs findings. A call in the other form is refused, both ways: a verifier's
that gives a `result`, and anyone else's that gives a `verdict` or `findings`,
so neither form ever stands in for the other. A reply without tool calls isn't
its verdict, so the plain-reply rule doesn't apply: it is nudged (`Check:
conclude`) to call `conclude`, and after two nudges in a row its next reply
concludes it with no verdict, a FAIL whatever the reply says ("verifier gave
no verdict", the reply as its findings): words it wouldn't type never pass.
What it is told names its verdict, never `conclude(result)`. Only the harness
starts one: `spawn_agent` refuses the type, and a run can't have one as its
root. A verifier started before the type existed is a standard agent; it keeps
working, re-engaged with the first-line form of the verdict.

The verifier must actually read the candidate. A message that lands while its
goroutine is on its way out (concluding, or stopping after a cancel) only bumps
that goroutine, and a respawn can fail without writing anything. So after
sending (or starting a new verifier, whose goroutine may fail before it creates
its session), the harness waits until the verifier is reading the candidate
(a turn of its began after it arrived, or it is running and its latest turn
is a working one) or is past it. Running alone isn't enough: a verifier
whose latest turn is a final one may be concluding with it, and one standing
by concludes quietly, waking no one, so the candidate would sit unread while
the agent waits with no timeout. If it is stopped or concluded without having
read it and no goroutine holds it, the harness sends again (which revives a
cold session), and if that doesn't take either, it concludes the agent, not
verified ("the verifier could not be started"). A goroutine that holds it
without reading it for 5 s is trusted (a slow start). A shutdown or a store failure during the attempt concludes nothing:
the attempt is left for the recovery.

The conclusion let through after a pass carries the `passed` record and a
`verified` note on the parent's `child_result`; when it ends without one,
`exhausted` (whose `reason` repeats the note) and a `not_verified` note saying
why (the latest findings, the verifier's stops, or that it couldn't be
started), both written atomically with the conclusion. Changes to one session's verification (set, clear, and
the harness's own steps) are serialized by a per-session lock in this
process (`verify.Lock`). A verifier is otherwise an ordinary sub-agent, with
five exceptions: its outcome reaches the agent as above, it stands by after a
withdrawal, the reminder doesn't count the agent's own verifier as left
running, `session_cancel` refuses an agent cancelling its own verifier, and
`send_message` refuses a message to it from the agent or from the agent's
other sub-agents (a message revives a concluded verifier, and its reply would
read as a new verdict; the agent reaches its verifier with `conclude`). Once
the agent has been told its verification ended without a pass, its next
conclusion is delivered not verified, with the ending it was told, whatever
the verifier said since (its parent, say, messaged it). After a restart, the
recovery marker says whether a running verifier is checking the agent's
conclusion or standing by. Verification can't be set on an interactive agent
or on a verifier; a request that names a field it doesn't have is refused.

**A `conclude` call open across a restart.** A call that was waiting when its
process stopped (or its session was cancelled) has no result. The next life
doesn't mend it as interrupted (see [Crash Recovery](#crash-recovery)): it runs the call
again, which finds its candidate with the verifier in the stream and waits on,
or settles a verdict that landed while the process was down. Recover respawns
such a session without a `RecoverEvent`, which would end the resumed wait at
once; its verifier is revived like any `ongoing` session. A call whose session
was cancelled meanwhile (a revival by a message) is answered as withdrawn by
the cancel instead, without running the checks, so the verifier the cancel
stopped stays stopped until the agent concludes again; one whose version
changed meanwhile (a `set`, `replace` or `clear` after the call in the stream,
which no `verifying` record followed: one that landed before the attempt took
the verification lock made the call's candidate the new version's) is answered
as superseded, so its candidate never goes to the new version. Only a turn
whose one call is `conclude` counts: alongside other calls it failed on its
own, and is mended like any other.

## Cascade on exit

| Exit | Owned children | Posts a result to its own parent |
|---|---|---|
| conclude (autonomous `conclude` call, or a reply taken as one) | cascade-cancel (`cancelSession`, reason `"parent concluded"`); the conclusion checks run first, so a held conclusion cancels nothing, and by a real conclusion its verifier has finished | yes — the return value |
| cancel | cascade-cancel (`cancelSession`) | yes — `cancelled` |
| crash | **kept alive** | yes — `crashed` (so an awaiting parent unblocks) |
| idle (interactive no-tool turn) | untouched | no |

**Crash keeps children alive.** A soft-crashed parent's children keep running and
post their results/crashes durably into the parent's stream; the parent consumes
them when it restarts. (A hard process crash leaves children `ongoing`-but-dead,
which Recover revives.) This stays coherent because the event stream — not
goroutine liveness — is the source of truth.

## Child results (exactly-once)

A child can stop and restart many times, so a parent's stream may hold many
`child_result`s for the same child. To keep delivery exactly-once **per
transition**:

- A terminal transition (`conclude`/`crashed`/`cancelled`) writes the
  `ChildResultEvent` into the parent's stream **in the same DB transaction** as
  the child's own status change (`TerminateAndNotifyParent`; a root agent skips
  the notify). The single SQLite DB makes this atomic, and the child posts from
  its own terminate path — no separate cross-goroutine hand-off.
- The spawner's runner is only a **fallback**: if the child goroutine exits with
  status still `ongoing` (a panic that bypassed `terminate`), the runner performs
  the same atomic crash-and-notify. On the normal path it posts nothing. A
  revived session's goroutine (`RunManager.runSessionGoroutine`) does the same
  on a panic, so a later life is covered as the first is.
- **Status is the dedupe token.** A child already in a terminal status was, by
  that atomicity, already notified; Recover/reconcile notifies only
  `ongoing`-but-dead children. No "has the parent seen this?" query is needed.
- `ChildResultEvent.Verdict` carries the outcome so the parent can branch
  (concluded → use result; crashed → maybe retry; cancelled → don't).

A child that crashes on its own (parent alive) still delivers a
`child_result(crashed)` so an awaiting parent unblocks rather than deadlocking.

## `await_event`

`await_event` parks the session until a *new* event arrives or a timeout fires.
It is **counter-based and level-triggered**, not a bare edge wait, so it is
immune to missed and spurious wakeups:

1. Snapshot the session's event counter **first** (`before`).
2. **Short-circuit**: if a peer event already landed at the current step during
   the just-finished LLM call, return immediately. In amplio's step model the
   `AssistantEvent` sits at the call step and peer events at `current_step`, so
   the check is `event_count(step >= current_step) >= 1`. (Snapshotting *before*
   this check closes the gap race.)
3. Otherwise park as `awaiting` and wait until the counter advances past
   `before`, or the timeout expires (mandatory, default 300 s). Re-check the
   counter on each wake so spurious/self wakes don't return early.
4. On a normal **wake**, restore `ongoing` — but only if the status isn't terminal
   (`SetStatusUnlessTerminal`, one conditional write, as for parking): a cancel
   may have interrupted the wait (via ctx) *or* flipped the status via a racing
   notify. Never clobber a terminal status (the canceller already set
   `cancelled`) with `ongoing` or `awaiting`.

`await_event` is **exclusive** (the only tool call in its turn) so no same-turn
self-write pollutes the counter. It wakes on *any* event (the `awaiting` row of
the matrix) but a record-only one; the `idle` park, by contrast, only advances
on `Input` events.

## Notification fabric

Waking a live session is **synchronous and lossless**: a commit-time hook runs
inside the event append (after the row commits, outside the store lock) and, for
the target session:

- if **live** → bumps its waiter counter, unless the event is record-only. The
  parked logic then decides whether to
  actually advance: `await` advances on any event; the `idle` park advances only
  on an `Input` event (re-sleeping otherwise).
- if **cold** → respawns iff the event is `Input` (`Classify`).

A dropped notification would mean a hang-until-timeout, so this path must never
drop. The waiter primitive is a monotonic counter guarded by a mutex plus a
broadcast channel that is closed-and-replaced on each notify; `WaitAfter`
snapshots the channel under the lock and selects on `{channel, timer, ctx}`.

## Spawn

When an agent spawns a child:

1. The child's `session_id` is allocated synchronously (a unique name reserved
   in memory — see below), so the parent gets the id back immediately.
2. The child's goroutine then creates its own DB session row and registry entry
   in `Run`/`bootstrap` (not pre-created in `spawn`). This leaves a tiny
   in-process window — between `spawn` returning the id and the child goroutine
   reaching `bootstrap`'s `CreateSession` — during which the session row does not
   yet exist, so an event targeting the child (e.g. a `send_message`) fails the
   append rather than landing. The window is a microsecond-scale goroutine
   scheduling gap that the normal flow never hits: the parent only learns the id
   from the tool result and acts on it a full LLM turn later, by which point the
   child has long since bootstrapped. The failed-append-in-the-gap case is an
   **accepted trade-off by design** (it fails loudly rather than mis-filing the
   event); closing it fully would mean splitting bootstrap across two goroutines
   for no practical benefit.
3. The child's goroutine runs with a context **independent of the parent's
   tool/loop context**, so the parent finishing, idle-exiting, or crashing does
   not cancel the child. Cancellation reaches the child only via `cancelSession`
   (which ctx-cancels through the registry handle).

The child's `session_id` is a unique nickname from the run's in-process **name
allocator**: a mutex-guarded used-set, seeded once from the DB so cold
(concluded/crashed/idle) names are never reused. Reserving the picked name in
memory at allocation time removes the pick→create race without a retry loop.

## Crash Recovery

The per-session **loop re-entry** mechanics — run whenever a cold session respawns,
whether via an `Input` restart or a [Recover](#recover) — repair crash artifacts and
are derived purely from DB state (see the table in
[`step_model.md`](step_model.md#crash-resume)). They are no-ops after a clean park:

- **Don't double-advance.** On the first iteration of a resumed run, advance the
  step unless resuming mid-LLM-call (`current_step > 0` and no `AssistantEvent`
  at it) — then call the LLM at the existing step so the `AssistantEvent` lands
  where the dead predecessor intended.
- **Repair orphan tool calls.** Synthesize a `ToolResultEvent` for every
  `tool_call` id with no matching result (crash mid-tool-execution), or the next
  LLM call is rejected for unmatched `tool_use`. Idempotent. The exception is
  the latest turn's `conclude` call (`db.OpenConclude`): it is **resumed**
  before the loop's first model call, run again from the stream (waiting on
  its verifier, or settling a verdict that came meanwhile), and its result
  completes its step. A session revived with such a call doesn't wait for an
  `Input` first: the call waits for whatever wakes it.
- **One-shot rest detection.** If the stream tail is already a no-tool
  `AssistantEvent` at resume (crashed after the final reply but before the status
  flip), finish it as the live loop would (the conclusion checks, then conclude)
  without a redundant LLM call — and [Recover](#recover) skips the `RecoverEvent`
  in this case, as for an open `conclude` call.
- **The "resumed" marker is the `RecoverEvent`** ([Recover](#recover) only). It is
  a user-role event (not a system message) because providers require the last
  pre-assistant message to be a user/tool-result message.
