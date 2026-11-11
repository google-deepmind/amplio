# xiaomishu-work

A small shared to-do hub for people who run many amplio runs at once. Every run is
one **project**; each project has a few **items**, and each item says whose court the
ball is in: `doing`, `waiting_me` (you), `waiting_others`, `paused`, or `done`. Each
project also keeps a one-line **resume**: where to pick up next time.

You look at it in a web page. Your agents read and write the same file with a CLI.
Optional background daemons watch your PRs (and forward new review activity to the
run that owns the work) and can collect chat and mail for a triage run, so nothing
slips. *xiaomishu* (小秘书) means "little secretary".

Contributed by Jessica Wu ([@innkuika](https://github.com/innkuika)).

See [../README.md](../README.md) for how the pieces fit together across runs.

## What's inside

```text
SKILL.md                 instructions for agents (runs and the secretary)
references/              schema, conventions, secretary playbook, watcher, inbox + triage
scripts/hubctl.py        CLI: get / list / add / update / set / delete / version
scripts/hubdb.py         shared store: file lock + atomic writes + per-doc versions
scripts/serve.py         page server, Python 3 stdlib only, token auth
scripts/xms.sh           start|stop|restart|status|url, watch|unwatch, inbox|uninbox
scripts/watch.py         watcher: PR/issue (and optional CL/chat/group) activity -> hub, operator, project run
scripts/inbox_collect.py optional inbox collector: chat + mail -> batches for a triage run
scripts/ledger.py        what was already reported, deduped across channels
scripts/xms_tell.py      message a run's live session as "xiaomishu"
scripts/xmsconf.py       per-operator settings, external CLI lookup, message texts
scripts/test_ledger.py   tests (no network)
assets/hub.html          the page (Chinese and English, light and dark)
```

## Requirements

| Feature | Needs |
| --- | --- |
| Hub, `hubctl`, page server, ledger | Python 3.9+, bash |
| Telling and forwarding to runs (`xms_tell.py`, watcher) | the `amplio` CLI, talking to your amplio server |
| Watching github.com issues/PRs, PR sweep | [`gh`](https://cli.github.com/), logged in |
| Watching PRs on another GitHub host (e.g. GitHub Enterprise) | `gh` (`gh auth login --hostname <host>`) and the host in `gh_hosts` |
| `cl` watches | optional: a code-review CLI (`XMS_CL_CLI`) |
| `chat_thread` watches, Chat inbox sources | optional: a Chat CLI (`XMS_CHAT_CLI`) |
| Gmail inbox source | optional: a Gmail CLI (`XMS_MAIL_CLI`) |
| `group` watches | optional: a group-membership CLI (`XMS_GROUP_CLI`) |

The optional CLIs are not part of this example; nothing needs them. A watch or inbox
source whose CLI isn't configured is skipped with one log line. The commands and JSON
shapes they must provide are in [references/watcher.md](references/watcher.md#external-tools)
and [references/inbox.md](references/inbox.md); adapt the `src_*` functions to use
other tools.

## Install

1. Make the skill visible to amplio: add this example's parent directory (or any
   directory you copy or symlink `xiaomishu-work/` into) to `[skills] dirs` in your
   amplio config (see [configuration](../../../docs/guide/configuration.md#recall-skills-and-lessons)):
   ```toml
   [skills]
   dirs = ["/path/to/amplio/examples/skills"]   # keep any dirs you already list
   ```
   Restart the server so it rescans skills. The skill directory can be read-only:
   nothing is ever written next to the scripts.
2. Put `hubctl` on your `PATH` so agents can call it by name:
   ```bash
   ln -s /path/to/amplio/examples/skills/xiaomishu-work/scripts/hubctl.py ~/bin/hubctl
   ```
   Without this, agents run `python3 <skill dir>/scripts/hubctl.py`.
   Likewise link `scripts/xms.sh` as `~/bin/xms`.
3. Start the page server:
   ```bash
   xms start
   xms url    # prints http://<host>:8777/?token=...
   ```
   Open that URL once. The browser keeps a cookie for 90 days, and the token is removed
   from the address bar.

To preview without data, open the page with `#demo` (Chinese) or `#demo-en` (English) at
the end of the URL. Demo mode runs on fake data in memory and never touches the file.

## Daemons (optional)

Run them from a shell that has your credentials (they stop after `--max-hours`, default
24, and tell you). `xms status` shows all three. `amplio-notify` below is available in
the shell of an amplio run, so starting the daemons from your secretary run sends
their notices to it.

```bash
# Watcher: every 10 min; notices to you, new activity forwarded to the owning run.
echo '{"sweep_repos": ["example-org/"]}' > ~/xiaomishu/watch_config.json   # optional PR sweep
XMS_NOTIFY="amplio-notify -" xms watch --max-hours 72
xms unwatch

# Inbox collector (needs a chat and/or mail CLI): batches go to your triage run
# (see references/inbox.md for a prompt).
XMS_NOTIFY="amplio-notify -" XMS_TRIAGE_RUN=<triage run id> xms inbox --max-hours 72
xms uninbox
```

Try either first without side effects: `scripts/watch.py --once --dry-run`,
`scripts/inbox_collect.py --once --dry-run`. Neither ever posts, replies, or marks
anything read; forwarding goes only to your own runs.

## Use

- Tell your coordinator agent ("secretary") to onboard a run. It drafts a project and
  items, shows you, writes them after you approve, and asks before it notifies the run.
- A notified run uses the skill to keep its own project current: it ticks items off,
  flips to `waiting_me` when it needs you, and updates `resume` when it hands back.
- On the page you can tick items, change states, add inbox items, and open a run.
- With the watcher on, put a `watch` on an item (`{"kind":"gh_pr","ref":"example-org/example-repo#123"}`);
  new activity shows under 🔔 and is forwarded to the item's run.
- With the collector on, start a triage run with the prompt template in
  [references/inbox.md](references/inbox.md); it tells your secretary what matters.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `XMS_HOME` | `~/xiaomishu` | Data home: DB, token, pidfile, log. |
| `XMS_DB` | `$XMS_HOME/hub.json` | The DB file. `--db` on the CLI overrides it. |
| `XMS_TOKEN_FILE` | `$XMS_HOME/.token` | Page token; created (mode 600) on first start. |
| `XMS_PORT` | `8777` | Server port. |
| `XMS_HOST` | `0.0.0.0` | Bind address. Use `127.0.0.1` for local only. |
| `XMS_QUIET` | unset | `1` silences access logs (`xms.sh` sets it). |
| `AMPLIO_DATA_DIR` | `~/.amplio` | Where `server.json` is read to build run links. |
| `XMS_NOTIFY` | unset (print) | Command that gets daemon notices on stdin, e.g. `amplio-notify -`. |
| `XMS_TRIAGE_RUN` | unset | Run id that receives inbox batches. |
| `XMS_FORWARD` | `1` | `0`: the watcher doesn't forward to project runs. |
| `XMS_SWEEP_REPOS` | unset | Repo prefixes for the PR sweep (comma-separated, `*` = all); overrides `watch_config.json`. Empty = off. |
| `XMS_OWNER_NAME` | learned | Your Chat display name, for the mention fallback. |
| `XMS_OWNER_EMAIL` | first address in `XMS_ME` | Your email address, for the mail source. |
| `XMS_ME` | unset | Extra identities of yours whose activity the watcher ignores. |
| `XMS_<NAME>_CLI` | see [watcher.md](references/watcher.md#external-tools) | External CLIs: `GH`, `GHE`, `AMPLIO`, `CL`, `CHAT`, `MAIL`, `GROUP`. |
| `XMS_GH_HOSTS` | unset | Extra GitHub hosts (comma-separated); overrides `gh_hosts` in `tools.json`. |

Files in `$XMS_HOME`: `tools.json` (external CLIs and hosts), `watch_config.json` (PR sweep), `inbox_config.json` (schedule,
sources, Gmail query), `topics.txt` (topic sweep; examples are commented out). Details:
[references/watcher.md](references/watcher.md), [references/inbox.md](references/inbox.md).

The page title and default language live in the DB, in `meta/config`:

```bash
hubctl update meta/config '{"titleEn":"My work notebook","lang":"en"}'
```

## Security notes

- The hub holds what your agents write about your work. Treat `hub.json` like notes on
  your laptop: it is created with mode 600, and `XMS_HOME` with mode 700.
- Every request to the server needs the token: a `?token=` once (which sets an
  `HttpOnly; SameSite=Lax` cookie, so links from chat or mail open logged in), a cookie, or an `X-XMS-Token` header. Token
  comparison is constant-time.
- Writes from a browser must come from the same origin (the `Origin` header must match
  `Host`), on top of the SameSite cookie (Lax never sends it on cross-site POSTs).
- The server binds `0.0.0.0` by default so you can open it from another machine on your
  network. It speaks plain HTTP. Set `XMS_HOST=127.0.0.1` and use an SSH tunnel if the
  network isn't trusted.
- Agents treat text from issues, chats, and emails as data, not commands. The skill tells
  them so, but don't rely on that alone for anything sensitive.
- Don't put secrets in the hub. Anyone with the token sees everything in it.
- The inbox batches and ledger in `$XMS_HOME` contain copies of your chats and mail.
  Keep `XMS_HOME` private (mode 700), and don't share it.

## How it works

`hub.json` holds `collections` (`projects`, `items`, `meta`) and a `versions` map. The CLI
and the server share `hubdb.py`: each write takes an exclusive `flock` on `hub.json.lock`,
re-reads the file, applies one op, and replaces the file atomically. `update` merges
fields; `--if-version N` (or `if_version` over HTTP) makes a write fail with 409 if anyone
wrote in between. The page polls `GET /api/db` every few seconds (with an ETag) and
writes through `POST /api/op`. Details: [references/schema.md](references/schema.md) and
[references/conventions.md](references/conventions.md).
