# Running amplio day to day

## One server per data directory

A `serve` process takes an exclusive lock on its data directory, so a second one
on the same directory refuses to start. When you do want a second amplio — a 
batch of experiments you would rather not mix with your day-to-day runs — give 
it its own directory and port:

```bash
AMPLIO_DATA_DIR=~/experiments AMPLIO_LISTEN=localhost:26760 amplio serve
```

The two are completely independent: separate databases, artifacts, model menus,
tokens and ports. Point any command at one with `--data-dir`:

```bash
amplio --data-dir=~/experiments client list
```

Two things a fresh directory does not inherit, both worth knowing before you
split: it starts with **no mined lessons** (they live in the database), and it
re-embeds the skill corpus on first start.

## Access and the token

The startup banner prints the URL with a token:

    amplio → http://localhost:26759/?token=abcdEFGH123some-long-token

**Reads are open; writes need the token.** Anyone who can reach the port sees
the read-only UI — every run, task, and transcript. The token authorises
mutations: starting runs, sending messages, cancelling, deleting.

Therefore, it is recommended to bind to loopback, and use SSH tunnel if you
need to access the web UI on a different machine.

If you are in an isolated team / corp network environment, where convenient
proxy (e.g. UberProxy) has been setup, you may use a routable address intead
of loopback, and directly use corp proxy access without having to manually
setup SSH tunnels. In this case, sharing your server address to collaborators 
within the same network (**without** the `?token=` part) gives them a readonly
view of your amplio runs.

Set `token` in `config.toml` to pin a known value; otherwise one is generated
into `<data-dir>/auth.token` on first start and reused.

### TLS

Browsers cap HTTP/1.1 at six connections per origin, and each open tab holds a
live event stream — so a few tabs can silently stall the UI. Serving HTTPS gets
HTTP/2 (and its multiplexing) for free. 

This is generally not an issue when operating via team / corp network environment,
where access via corp proxy provides TLS by default. But when operating in localhost
or via SSH tunnel may benefit setting up TLS following [TLS and HTTP/2](../internals/tls.md),
especially when you need to operate more than a few runs simultaneously.

## Restarts and recovery

Configuration is read at startup, so any change — a model, a skill directory, a
new briefing — takes effect on the next `serve`.

Restarting is safe. On boot amplio revives the sessions that were still working
when the process ended, and logs what it scheduled:

    level=INFO msg="recovery scheduled" sessions=3

Individual runs can also be revived on demand with `amplio client restart
<run-id>`, which is the same path.

Amplio is designed to be robust to server restarts: on-going tool calls will
be canceled. The agents receive tool call cancelation feedback and decide
to re-issue them (or not). The harness never auto retry tool calls.

## Disk, backups and cleanup

Everything lives under the data directory:

| what | where | notes |
|---|---|---|
| runs, sessions, events | `amplio.db` | one SQLite file; the source of truth |
| per-run scratch files | `artifacts/<run-id>/` | created with the run |
| tool-result blobs | `blobs/<run-id>/` | images and similar, kept out of the DB |
| server logs | `logs/` | one file per `serve` invocation |

**Back up the database through SQLite, not with `cp`.** Amplio runs in WAL mode,
so a plain copy of `amplio.db` while the server is up can miss committed data
still sitting in the write-ahead log. Either tool works, and both are safe
against a live server:

```bash
sqlite3 ~/.amplio/amplio.db ".backup '/backup/amplio-$(date +%F).db'"
```

```bash
python3 - <<'EOF'
import sqlite3
src = sqlite3.connect("file:$HOME/.amplio/amplio.db?mode=ro", uri=True)
dst = sqlite3.connect("/backup/amplio.db")
src.backup(dst)
EOF
```

Artifacts and blobs are ordinary files; copy them the ordinary way.

Deleting a run removes its database rows and its artifact and blob directories.
Lessons mined from it survive — they are corpus, not run data. Deletion is
irreversible; archiving is the reversible way to get a run out of your list.

## Logs

One file per `serve`, named for its start time, under `<data-dir>/logs/`. Raise
the level when diagnosing:

```bash
amplio serve --log-level=debug     # or $AMPLIO_LOG_LEVEL
amplio serve --log-format=json     # structured, for shipping elsewhere
```

## A caution about what agents can do

Amplio does **not** sandbox the agent's shell. An agent runs commands as the
user that started the server, with that user's filesystem access, network access
and credentials. Run it somewhere you are comfortable with that: a container, a
VM, or a machine whose blast radius you accept.
