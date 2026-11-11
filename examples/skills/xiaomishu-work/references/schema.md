# Data schema

One JSON file (default `$XMS_HOME/hub.json`, mode 600):

```json
{
  "collections": {
    "projects": {"<run_id>": {...}},
    "items":    {"<id>": {...}},
    "meta":     {"config": {...}}
  },
  "versions": {"projects/<run_id>": 3, "items/<id>": 1}
}
```

`versions` holds one integer per doc, bumped by every write. A missing doc has
version 0. Only the store touches `versions`; never put it in a doc.

All fields are optional unless marked required. Dates are `YYYY-MM-DD`; timestamps
are ISO 8601 (`2026-10-27T23:10:00Z`). Unknown fields are kept and ignored.

## `projects/<run_id>`

The doc id is the amplio run id, so a run finds its project with `$AMPLIO_RUN_ID`.

| Field | Type | Meaning |
| --- | --- | --- |
| `title` | string, required | Short name. A leading emoji is shown as the icon. |
| `emoji` | string | Icon if the title has none. |
| `run_id` | string | Same as the doc id. The page builds the run link from it. |
| `stage` | enum | Ball state, see below. |
| `summary` | string | One or two sentences: what this project is. |
| `resume` | string | One sentence: where to pick up next time. |
| `waitingFor` | string | Who or what the project waits on (with `waiting_others`). |
| `links` | link[] | CLs, PRs, chats, bugs, docs. |
| `lastActive` | timestamp | Last time anyone moved this project. Drives auto-parking. |
| `starred` | bool | Pinned to the top of its group. |
| `area` | string | Free-form grouping label, such as `infra`. |
| `doneAt` | date | When the project finished (with `stage: done`). |

### Project `stage`

| Value | Shown as | Meaning |
| --- | --- | --- |
| `doing` | 🔄 In progress | Someone is working on it. Default if missing or unknown. |
| `waiting_me` | ⏳ Waiting on you | The operator must act. Shown first on the Today tab. |
| `waiting_others` | ⏳ Waiting on others | Blocked on someone else; fill `waitingFor`. |
| `paused` | 🧊 Parked | Parked by hand. |
| `done` | ✅ Done | Finished. |
| `idea` | Idea | Not started; optional. |

A project that is not `done` and has had no activity for 21 days (latest of
`lastActive` and its items' `doneAt`) is shown as parked even if its `stage` says
otherwise. The page lists parked projects with open items first ("loose ends").

## `items/<id>`

Ids are random 8-hex strings from `hubctl add items`, unless you choose one.

| Field | Type | Meaning |
| --- | --- | --- |
| `title` | string, required | One line. |
| `project` | run id or null | Owning project. `null` or missing = inbox. |
| `step` | int | Order within the project. Open `todo` items after the first open step show as locked. |
| `status` | enum | See below. |
| `waitingOn` | string | Who the item waits on (with `waiting_others`). |
| `due` | date or null | Due date. |
| `note` | string | Short extra context; the row expands to show it. |
| `links` | link[] | As on projects. |
| `watch` | object | Something to watch for news: `{kind, ref, seen, ...}`, see below. |
| `news` | bool | The watched thing changed since `watch.seen`, see below. |
| `created` | date or timestamp | When the item was added. |
| `doneAt` | date | Day it was actually done or skipped. |

### Item `status`

| Value | Meaning |
| --- | --- |
| `todo` | Open, ball with whoever is working on it. |
| `waiting_me` | The operator must act. |
| `waiting_others` | Blocked on someone else; fill `waitingOn`. |
| `done` | Finished; set `doneAt`. |
| `skipped` | Dropped. Not owed; set `doneAt`. |

### `watch`

One object per item (not a list): to watch two things, use two items. The operator or
an agent sets `kind`, `ref` and optionally `seen`; the watcher
([watcher.md](watcher.md)) maintains the rest.

| Field | Set by | Meaning |
| --- | --- | --- |
| `kind` | writer | `gh_issue`, `gh_pr`, `cl`, `chat_thread`, or `group`. |
| `ref` | writer | What to watch; formats below. |
| `seen` | writer / page / watcher | Timestamp of the last state the operator saw. Missing = the watcher's first look sets it to now (a baseline, no news). Only activity after `seen` counts. |
| `checked` | watcher | When the watcher last looked. |
| `latest` | watcher | Newest unseen activity: `{who, at, text, url, count}` (`count` = events since `seen`). The page shows it under 🔔. |
| `notified` | watcher | Timestamp (full precision) of the newest event already notified and forwarded, so it is never sent twice. |
| `once` | watcher | One-shot events already reported, e.g. `["submitted"]` (CL), `["joined"]` (group). |
| `error` | watcher | Last fetch error, or `null`. The page shows ⚠️ with it as a tooltip. |

`ref` formats:

| `kind` | `ref` |
| --- | --- |
| `gh_issue`, `gh_pr` | `owner/repo#123` (github.com) or a full URL. Other GitHub hosts (e.g. `https://git.example.com/o/r/pull/7`) must be listed in `gh_hosts`, see [watcher.md](watcher.md#external-tools). |
| `cl` | `1234567`. New published comments, and submit (once). Needs the optional code-review CLI. |
| `chat_thread` | `spaces/AAAA`. Watches the **whole space**; avoid it for DMs and busy spaces. Needs the optional Chat CLI. |
| `group` | A group name. Fires once when the operator joins the group. Needs the optional group CLI. |

### `news`

`true` when the watched thing has activity the operator hasn't seen. The page lists the
item under 🔔 New activity; tapping "Seen" sets `news: false` and moves `watch.seen`
to now. Only the watcher sets it to `true`.

## Link

```json
{"kind": "gh", "label": "#123", "url": "https://github.com/example-org/example-repo/pull/123"}
```

`kind` is one of `cl`, `gh`, `chat`, `bug`, `doc`, `run`. Only `http(s)://` and
root-relative URLs are rendered as links. A `run` link is optional: without one,
the page uses `runBase + /runs/<run_id>/chat`.

## `meta/config`

| Field | Meaning |
| --- | --- |
| `title` | Page title in Chinese mode. Default "我的工作小本本". |
| `titleEn` | Page title in English mode. Default "My work notebook". |
| `lang` | Default UI language, `zh` or `en`. The page's 中/EN toggle overrides it per browser. |
| `runBase` | Base URL of the amplio server. `serve.py` injects it on every read from `$AMPLIO_DATA_DIR/server.json` (default `~/.amplio/server.json`), so links survive a port change. It is not written back to the file. |
