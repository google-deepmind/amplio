#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""xiaomishu-work watcher: flags items whose watched thing has new activity.

For every open item with a `watch` field it polls the source, and when someone
other than the owner did something after `watch.seen`, it sets `news: true` and
`watch.latest = {who, at, text, url}` on the item (the page then shows it under
"New activity"), and sends ONE batched notification per round via --notify.

watch.kind / watch.ref (CLIs resolve via xmsconf.tool: $XMS_<NAME>_CLI or $XMS_HOME/tools.json):
  gh_issue, gh_pr  github.com:  "owner/repo#123" or a full github.com URL (`gh`)
                   other hosts: a full URL, e.g. https://git.example.com/o/r/pull/7
                   (`gh --hostname`, or the "ghe" CLI if configured)
  cl               "1234567"                   (optional "cl" code-review CLI: comments, submit)
  chat_thread      "spaces/AAAA"               (optional "chat" CLI: new messages in the space)
  group            group name                  (optional "group" CLI: fires once when you join)
A watch whose CLI isn't configured is skipped with one log line; it never fails the round.

Usage: watch.py [--interval 600] [--max-hours 24] [--once [--dry-run]] [--notify CMD] [--db PATH]
  --notify CMD   shell command that gets the message on stdin (e.g. "amplio-notify -").
  New activity is also forwarded to the item's project run (projects/<id>.run_id) via
  amplio notify, with the push/reply policy in xmsconf.TEXTS; XMS_FORWARD=0 disables.
  PR sweep: each round, open PRs authored by (or requesting review from) the owner in the repos
  listed in $XMS_SWEEP_REPOS or $XMS_HOME/watch_config.json "sweep_repos" get a watch; empty = off.
  Owner identities (activity by these is ignored): $XMS_ME (comma-separated),
  plus $USER and the `gh` login.  Stops after --max-hours (and says so), or when
  $XMS_HOME/.watch.stop exists.  A pidfile prevents two watchers on one DB.
"""

import sys
sys.dont_write_bytecode = True

import argparse
import datetime as dt
import json
import os
import re
import subprocess
import time

sys.path.insert(0, os.path.dirname(os.path.realpath(__file__)))
import hubdb  # noqa: E402
import ledger  # noqa: E402
import xmsconf  # noqa: E402

CLOSED = {"done", "skipped"}


def now_iso():
  return dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_ts(v):
  """ISO string or epoch seconds -> aware datetime (None if unparseable)."""
  if v in (None, ""):
    return None
  try:
    if isinstance(v, (int, float)) or re.fullmatch(r"\d{9,}", str(v)):
      return dt.datetime.fromtimestamp(int(v), dt.timezone.utc)
    s = str(v).replace("Z", "+00:00")
    s = re.sub(r"(\.\d{6})\d+", r"\1", s)  # nanoseconds -> micro
    d = dt.datetime.fromisoformat(s)
    return d if d.tzinfo else d.replace(tzinfo=dt.timezone.utc)
  except ValueError:
    return None


def iso(d):
  return d.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def run(cmd, timeout=90):
  p = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
  if p.returncode != 0:
    # Keep the whole tail: multi-line errors (e.g. a 429 with a JSON "Details: [...]" block)
    # used to collapse to "]", which hid the rate limit and skipped the backoff fallback.
    err = " ".join((p.stderr or p.stdout or "failed").split())
    raise RuntimeError(err[-400:])
  return p.stdout


def run_json(cmd, timeout=90):
  out = run(cmd, timeout)
  try:
    v = json.loads(out)
  except ValueError:
    raise RuntimeError(f"non-JSON output from {cmd[0]} (login expired?)")
  return [] if v is None else v  # several CLIs print `null` for "no results"


class Me:
  def __init__(self):
    ids = {x.strip().lower() for x in os.environ.get("XMS_ME", "").split(",") if x.strip()}
    if os.environ.get("USER"):
      ids.add(os.environ["USER"].lower())
    try:
      ids.add(run([xmsconf.need("gh"), "api", "user", "-q", ".login"], 30).strip().lower())
    except Exception:  # pylint: disable=broad-except
      pass
    self.ids = {i for i in ids if i}

  def __call__(self, who):
    w = (who or "").lower()
    return any(w == i or w.startswith(i + "@") or w.endswith("/" + i) for i in self.ids)


# ---- sources: each returns a list of events {who, at(datetime), text, url} ----

def _gh_target(ref):
  """-> (owner/repo, number, host)."""
  m = re.match(r"https?://([^/]+)/([^/]+/[^/]+)/(?:issues|pull|pulls)/(\d+)", ref)
  if m:
    host, repo, num = m.groups()
    return repo, num, host.lower()
  m = re.match(r"([^/#\s]+/[^/#\s]+)#(\d+)$", ref)
  if m:
    return m.group(1), m.group(2), "github.com"
  raise RuntimeError(f"can't parse GitHub ref {ref!r}")


def _gh_cli(host):
  """argv prefix for `<cli> api ...` against `host`."""
  if host == "github.com":
    return [xmsconf.need("gh"), "api"]
  ghe = xmsconf.tool("ghe")
  return [ghe, "api"] if ghe else [xmsconf.need("gh"), "api", "--hostname", host]


def _login(user, default="ghost"):
  """GitHub user object -> login; deleted accounts come back as null."""
  return (user or {}).get("login") or default


def src_github(ref, since, is_pr):
  repo, num, host = _gh_target(ref)
  api = _gh_cli(host)
  base = f"repos/{repo}"
  q = f"?per_page=100&since={iso(since)}"
  ev = []
  for c in run_json(api + [f"{base}/issues/{num}/comments{q}"]):
    ev.append(dict(who=_login(c.get("user")), at=parse_ts(c["created_at"]), text=c.get("body") or "",
                   url=c.get("html_url")))
  issue = run_json(api + [f"{base}/issues/{num}"])
  if is_pr or issue.get("pull_request"):
    for r in run_json(api + [f"{base}/pulls/{num}/reviews?per_page=100"]):
      if r.get("submitted_at"):
        ev.append(dict(who=_login(r.get("user")), at=parse_ts(r["submitted_at"]),
                       text=f"[{r.get('state','').lower()}] {r.get('body') or ''}", url=r.get("html_url")))
    for c in run_json(api + [f"{base}/pulls/{num}/comments{q}"]):
      ev.append(dict(who=_login(c.get("user")), at=parse_ts(c["created_at"]), text=c.get("body") or "",
                     url=c.get("html_url")))
    pr = run_json(api + [f"{base}/pulls/{num}"])
    if pr.get("merged_at"):
      ev.append(dict(who=_login(pr.get("merged_by"), "?"), at=parse_ts(pr["merged_at"]),
                     text="[merged]", url=pr.get("html_url"), key=f"{ledger.key_url(pr.get('html_url'))}#merged"))
    elif pr.get("closed_at"):  # closed without merging
      ev.append(dict(who=_login(issue.get("closed_by"), "?"), at=parse_ts(pr["closed_at"]),
                     text="[closed]", url=pr.get("html_url"), key=f"{ledger.key_url(pr.get('html_url'))}#closed"))
  elif issue.get("closed_at"):
    ev.append(dict(who=_login(issue.get("closed_by"), "?"), at=parse_ts(issue["closed_at"]),
                   text="[closed]", url=issue.get("html_url"), key=f"{ledger.key_url(issue.get('html_url'))}#closed"))
  return ev


def cl_url(num):
  """Link for a CL: tools.json "cl_url" template with {num}, else None."""
  t = xmsconf.tool_setting("cl_url")
  return t.format(num=num) if t else None


def src_cl(ref, since):
  cli = xmsconf.need("cl")
  num = re.sub(r"\D", "", str(ref))
  url = cl_url(num)
  ev = []
  for c in run_json([cli, "readonly", "list-comments", f"--cl={num}", "--json", "--limit=200"]):
    if c.get("published") is False:
      continue
    ev.append(dict(who=c.get("author", "?"), at=parse_ts(c.get("publishTimestamp") or c.get("timestamp")),
                   text=c.get("content") or "", url=url, key=ledger.key_cl(num, c.get("id"))))
  head = run([cli, "readonly", "info", f"--cl={num}"]).splitlines()[:1]
  if head and "*submitted*" in head[0]:
    ev.append(dict(who="submit", at=dt.datetime.now(dt.timezone.utc), text="[submitted]", url=url, once="submitted",
                  key=f"cl:{num}:submitted"))
  return ev


def src_chat(ref, since):
  cli = xmsconf.need("chat")
  space = str(ref).split("/")[1] if str(ref).startswith("spaces/") else str(ref)
  url = f"https://chat.google.com/room/{space}"
  ev = []
  try:
    msgs = run_json([cli, "readonly", "list-messages", "--space", space, "--max", "30", "--json"], 120)
  except RuntimeError as e:
    if "rateLimit" not in str(e) and "429" not in str(e):
      raise
    # Quota hit: the stream variant retries 429s with jittered backoff.
    p = subprocess.run([cli, "readonly", "stream", "list-messages", "-i", "-", "--window", "7d"],
                       input=json.dumps({"space_id": space}), capture_output=True, text=True, timeout=240)
    rows = [json.loads(l) for l in p.stdout.splitlines() if l.strip()]
    if not rows or not rows[0].get("_ok"):
      raise RuntimeError("chat quota exceeded (retry next round)")
    msgs = rows[0].get("data") or []
  for m in msgs:
    s = m.get("sender") or {}
    who = s.get("email") or s.get("display_name") or s.get("name") if isinstance(s, dict) else str(s)
    ev.append(dict(who=who or "?", at=parse_ts(m.get("create_time")), text=m.get("text") or "",
                   url=m.get("url") or url, name=(s.get("display_name") if isinstance(s, dict) else None),
                   chat_name=m.get("name")))
  return ev


def src_group(ref, since):
  """Group membership: one event the first time the owner shows up in the group.

  Runs `<group CLI> membership show --parent <ref> --child <user>` and treats output
  containing "Membership" as joined. User: $XMS_GROUP_USER > tools.json group_user > $USER."""
  cli = xmsconf.need("group")
  user = (os.environ.get("XMS_GROUP_USER") or xmsconf.tool_setting("group_user")
          or os.environ.get("USER", ""))
  p = subprocess.run([cli, "membership", "show", "--parent", ref, "--child", user],
                     capture_output=True, text=True, timeout=60)
  if "Unable to build RPC request" in (p.stdout + p.stderr):
    raise RuntimeError((p.stdout + p.stderr).strip().splitlines()[0][:160])
  if "Membership" in p.stdout:
    t = xmsconf.tool_setting("group_url")
    return [dict(who="group", at=dt.datetime.now(dt.timezone.utc), text=f"[joined {ref}]",
                 url=t.format(group=ref.lstrip("%")) if t else None, once="joined",
                 key=f"group:{ref}:joined")]
  return []


def fetch(w, since):
  kind, ref = w.get("kind"), str(w.get("ref") or "")
  if kind in ("gh_issue", "gh_pr"):
    return src_github(ref, since, kind == "gh_pr")
  if kind == "cl":
    return src_cl(ref, since)
  if kind == "chat_thread":
    return src_chat(ref, since)
  if kind == "group":
    return src_group(ref, since)
  raise RuntimeError(f"unknown watch kind {kind!r}")


# ---- forwarding to the owning run ----

def amplio_session(run_id):
    """The run's live top-level session: newest non-concluded root, else root_session_id."""
    run = run_json([xmsconf.need("amplio"), "client", "api", f"/api/runs/{run_id}"], 30)
    roots = [x for x in run.get("sessions") or [] if not x.get("parent_id")]
    live = [x for x in roots if x.get("status") not in ("concluded", "crashed", "cancelled")]
    pick = max(live or roots, key=lambda x: x.get("status_changed_at") or "", default=None)
    return (pick or {}).get("session_id") or run.get("root_session_id")


def forward(run_id, item_id, it, ev_list, lang="en"):
    if not run_id or os.environ.get("XMS_FORWARD", "1") == "0":
        return None
    sid = amplio_session(run_id)
    if not sid:
        return "no session"
    lines = [xmsconf.text(lang, "fwd_head", iid=item_id, title=it.get("title", ""))]
    for e in ev_list[-5:]:
        who = e.get("name") or e["who"]
        body = re.sub(r"\s+", " ", e.get("text") or "").strip()[:1200]
        lines.append(f"- {who}（{iso(e['at'])}）: {body}\n  {e.get('url') or ''}")
    lines.append(xmsconf.text(lang, "fwd_policy"))
    body = json.dumps({"sender": "xiaomishu", "content": "\n".join(lines)}, ensure_ascii=False)
    p = subprocess.run([xmsconf.need("amplio"), "client", "api", "-X", "POST",
                        f"/api/runs/{run_id}/sessions/{sid}/notify", "--data", "-"],
                       input=body, capture_output=True, text=True, timeout=30)
    return None if p.returncode == 0 else (p.stderr or p.stdout).strip()[:160]


# ---- one round ----

def _unreported(ev_list):
  """Events whose ledger key isn't recorded yet. Inbox entries deferred to the watcher
  (covered_by set) don't count: the watcher still owns forwarding those to project runs."""
  keys = [ledger.event_key(e) for e in ev_list]
  done = ledger.seen(keys, ignore=lambda r: r.get("by") == "inbox" and r.get("covered_by"))
  return [e for e, k in zip(ev_list, keys) if not k or k not in done]


def check_round(db_path, me, errors_seen, fails=None, dry_run=False):
  """Returns a list of human-readable lines worth notifying about.

  dry_run: no forwarding, no ledger writes, no hub writes."""
  merge = (lambda *_: None) if dry_run else _merge_watch
  db = hubdb.read(db_path)
  items = db["collections"].get("items", {})
  projects = db["collections"].get("projects", {})
  lang = xmsconf.lang(db)
  lines = []
  fails = {} if fails is None else fails
  for iid, it in sorted(items.items()):
    w = _watch_obj(it)
    if not it.get("watch") or it.get("status") in CLOSED:
      continue
    if not w or not w.get("kind"):
      if f"{iid}:shape" not in errors_seen:  # say it once per process, not every round
        errors_seen.add(f"{iid}:shape")
        lines.append(f"⚠️ items/{iid} {it.get('title','')[:40]}: watch must be one {{kind, ref}} object "
                     "(one item per watched thing); ignored")
      continue
    seen = parse_ts(w.get("seen"))
    patch = {"checked": now_iso()}
    if seen is None:  # first look: baseline, no news
      patch["seen"] = now_iso()
      merge(db_path, iid, patch, None)
      continue
    try:
      ev = fetch(w, seen)
      patch["error"] = None
      errors_seen.discard(iid)
      fails.pop(iid, None)
    except xmsconf.ToolMissing as e:
      # Optional source not installed: skip quietly (one log line per kind), not an error.
      if f"tool:{e.name}" not in errors_seen:
        errors_seen.add(f"tool:{e.name}")
        print(f"{now_iso()} skipping {w.get('kind')} watches: {e}", flush=True)
      patch["error"] = f"skipped: {e}"
      merge(db_path, iid, patch, None)
      continue
    except Exception as e:  # pylint: disable=broad-except
      msg = str(e)[:200]
      patch["error"] = msg
      merge(db_path, iid, patch, None)
      fails[iid] = fails.get(iid, 0) + 1
      if fails[iid] >= 2 and iid not in errors_seen:  # one blip is normal; tell on the 2nd in a row
        errors_seen.add(iid)
        lines.append(f"⚠️ {it.get('title','')[:40]}: can't check {w.get('kind')} ({msg})")
      continue
    once_done = set(w.get("once") or [])
    fresh = []
    for e in ev:
      if e.get("once"):
        if e["once"] not in once_done:
          fresh.append(e)
          once_done.add(e["once"])
      elif e["at"] and e["at"] > seen and not me(e["who"]):
        fresh.append(e)
    notified = parse_ts(w.get("notified"))
    fresh.sort(key=lambda e: e["at"] or seen)
    if fresh:
      last = fresh[-1]
      who = last.get("name") or last["who"]
      text = re.sub(r"\s+", " ", last["text"]).strip()[:160]
      patch.update(latest={"who": who, "at": iso(last["at"]), "text": text, "url": last.get("url"),
                           "count": len(fresh)},
                   once=sorted(once_done))
      is_new = notified is None or last["at"] > notified or len(once_done) > len(w.get("once") or [])
      # Full precision: second-truncated marks made sub-second events look new again.
      patch["notified"] = max(last["at"], notified or last["at"]).isoformat()
      merge(db_path, iid, patch, True)
      new_ev = [e for e in fresh if e.get("once") or notified is None or (e["at"] and e["at"] > notified)]
      unrep = _unreported(new_ev or fresh) if is_new else []
      if is_new and not unrep:
        print(f"{now_iso()} {iid}: {len(new_ev or fresh)} new event(s) already in ledger; not notifying", flush=True)
      if unrep:
        proj = projects.get(it.get("project") or "", {})
        last = unrep[-1]
        who = last.get("name") or last["who"]
        text = re.sub(r"\s+", " ", last["text"]).strip()[:160]
        if dry_run:
          ferr = None
          print(f"[dry-run] would forward to run {proj.get('run_id')}: "
                + "; ".join(f"{ledger.event_key(e)}" for e in unrep), flush=True)
        else:
          try:
            ferr = forward(proj.get("run_id"), iid, it, unrep, lang=lang)
          except Exception as fe:  # pylint: disable=broad-except
            ferr = str(fe)[:160]
          ledger.record_many([ledger.event_key(e) for e in unrep], "watcher", item=iid)
        fwd = "" if not proj.get("run_id") else (
            xmsconf.text(lang, "fwd_ok") if not ferr else xmsconf.text(lang, "fwd_fail", err=ferr))
        fwd = " [dry-run]" if dry_run else fwd
        tag = f"[{proj.get('title','')[:24]}] " if proj else ""
        more = f" (+{len(unrep)-1})" if len(unrep) > 1 else ""
        lines.append(f"🔔 {tag}{it.get('title','')[:50]} — {who}: {text[:100]}{more}{fwd}")
    else:
      merge(db_path, iid, patch, None)
  return lines


def sweep_my_prs(db_path, dry_run=False, log_disabled=True):
  """Make sure every open PR authored by the owner (in the configured repos) is watched by some open item.
  Attaches a watch to an item that already mentions the PR (#N in title/links), else files an inbox item.
  Returns human-readable lines for anything it added (or would add, with dry_run). Never raises."""
  home = os.path.dirname(db_path)
  allow = xmsconf.sweep_repos(home)
  if os.environ.get("XMS_SWEEP_PRS", "1") == "0" or not allow:
    if log_disabled:
      print(f"{now_iso()} PR sweep off (set XMS_SWEEP_REPOS or \"sweep_repos\" in "
            f"{os.path.join(home, 'watch_config.json')}; \"*\" = all repos)", flush=True)
    return []
  try:
    gh = xmsconf.need("gh")
    prs = run_json([gh, "search", "prs", "--author=@me", "--state=open", "--limit", "100",
                    "--json", "repository,number,title,url"], 60)
    if xmsconf.watch_config(home).get("sweep_review_requests"):
      for pr in run_json([gh, "search", "prs", "--review-requested=@me", "--state=open", "--limit", "50",
                          "--json", "repository,number,title,url"], 60) or []:
        pr["_review"] = True
        prs.append(pr)
  except Exception as e:  # pylint: disable=broad-except
    print(f"{now_iso()} PR sweep skipped: {e}", file=sys.stderr, flush=True)
    return []
  db = hubdb.read(db_path)
  lang = xmsconf.lang(db)
  items = db["collections"].get("items", {})
  open_items = {k: v for k, v in items.items() if v.get("status") not in CLOSED}
  watched = set()
  for it in open_items.values():
    w = _watch_obj(it) or {}
    if w.get("kind") in ("gh_pr", "gh_issue"):
      try:
        repo, num, _ = _gh_target(str(w.get("ref") or ""))
        watched.add((repo.lower(), str(num)))
      except RuntimeError:
        pass
  added = []
  for pr in prs:
    repo, num = pr["repository"]["nameWithOwner"], str(pr["number"])
    if "*" not in allow and not any(repo.lower().startswith(a) for a in allow):
      continue
    if (repo.lower(), num) in watched:
      continue
    ref = f"{repo}#{num}"
    pat = re.compile(rf"#{num}\b|/pull/{num}\b")
    host = None
    for k, it in sorted(open_items.items()):
      blob = (it.get("title") or "") + " " + json.dumps(it.get("links") or [])
      if pat.search(blob) and not it.get("watch"):
        host = k
        break
    pre = "[dry-run] would " if dry_run else ""
    try:
      if host:
        if not dry_run:
          hubdb.run_op(db_path, "update", f"items/{host}", {"watch": {"kind": "gh_pr", "ref": ref, "seen": now_iso()}})
        added.append(f"👀 {pre}auto-watch {ref} on items/{host}")
      elif dry_run:
        added.append(f"👀 {pre}auto-watch {ref} (new inbox item)")
      else:
        r = hubdb.run_op(db_path, "add", "items", {
            "title": (f"Review PR {ref}: " if pr.get("_review") else f"PR {ref}: ") + pr.get('title','')[:80],
            "project": None, "status": "waiting_me" if pr.get("_review") else "waiting_others",
            "links": [{"kind": "gh", "label": f"#{num}", "url": pr.get("url")}],
            "note": xmsconf.text(lang, "sweep_note"),
            "created": dt.date.today().isoformat(), "doneAt": None,
            "watch": {"kind": "gh_pr", "ref": ref, "seen": now_iso()}})
        added.append(f"👀 auto-watch {ref} (new inbox item {r.get('id')})")
      watched.add((repo.lower(), num))
    except Exception as e:  # pylint: disable=broad-except
      print(f"{now_iso()} PR sweep failed for {ref}: {e}", file=sys.stderr, flush=True)
  return added


def _watch_obj(it):
  """The item's watch object; a one-element list (hand-written by agents) counts, else None."""
  w = it.get("watch")
  if isinstance(w, list) and len(w) == 1:
    w = w[0]
  return w if isinstance(w, dict) else None


def collector_health(home, lang="en"):
  """Returns a warning string if the inbox collector looks dead or stalled, else None."""
  pidf = os.path.join(home, ".inbox.pid")
  logf = os.path.join(home, ".inbox.log")
  if not os.path.exists(os.path.join(home, "inbox_state.json")):
    return None  # collector not set up here
  try:
    pid = int(open(pidf).read().strip())
    os.kill(pid, 0)
  except (OSError, ValueError):
    return xmsconf.text(lang, "inbox_down")
  try:
    age = (time.time() - os.path.getmtime(logf)) / 60
  except OSError:
    return None
  import inbox_collect  # pylint: disable=g-import-not-at-top  (it imports this module)
  cfg = {**inbox_collect.DEFAULTS, **xmsconf.load_json(os.path.join(home, "inbox_config.json"), {})}
  tz = inbox_collect.zoneinfo.ZoneInfo(cfg["tz"]) if cfg["tz"] else None
  every = inbox_collect.every_minutes(cfg, dt.datetime.now(tz) if tz else dt.datetime.now().astimezone())
  # Complain only when the gap is clearly longer than the collector's current schedule.
  if every and age > max(2.5 * every, 25):
    return xmsconf.text(lang, "inbox_stalled", min=int(age))
  return None


def _merge_watch(db_path, iid, patch, news):
  """Merges `patch` into items/<iid>.watch under the DB lock (never clobbers seen).
  A one-element list watch is rewritten as the plain object."""
  def fn(db):
    it = db["collections"].get("items", {}).get(iid)
    w = _watch_obj(it or {})
    if w is None:
      return None
    w = dict(w)
    for k, v in patch.items():
      if k == "seen" and w.get("seen"):
        continue  # the page/owner may have just moved it
      w[k] = v
    data = {"watch": w}
    if news is not None:
      data["news"] = news
    return hubdb.apply_op(db, "update", f"items/{iid}", data)
  try:
    hubdb.transact(db_path, fn)
  except hubdb.OpError:
    pass


def _text(db_path, key, **kw):
  return xmsconf.text(xmsconf.lang(hubdb.read(db_path)), key, **kw)


def notify(cmd, text):
  if not cmd:
    print(text, flush=True)
    return
  p = subprocess.run(cmd, shell=True, input=text, text=True, capture_output=True)
  if p.returncode == 3 or "env_notice_capped" in (p.stderr or ""):
    print("notify capped; dropping", file=sys.stderr)


def main(argv=None):
  ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
  ap.add_argument("--interval", type=int, default=600)
  ap.add_argument("--max-hours", type=float, default=24)
  ap.add_argument("--once", action="store_true")
  ap.add_argument("--notify", default=os.environ.get("XMS_NOTIFY", ""))
  ap.add_argument("--db", default=None)
  ap.add_argument("--dry-run", action="store_true", help="with --once: print only; no forwarding, no writes")
  a = ap.parse_args(argv)
  db_path = hubdb.db_path(a.db)
  home = os.path.dirname(db_path)
  os.environ.setdefault("XMS_LEDGER", os.path.join(home, "ledger.jsonl"))
  stop_file = os.path.join(home, ".watch.stop")
  pidf = os.path.join(home, ".watch.pid")
  errors_seen, fails = set(), {}
  if a.dry_run and not a.once:
    ap.error("--dry-run needs --once")
  if a.once:
    me = Me()
    for l in sweep_my_prs(db_path, dry_run=a.dry_run) + check_round(db_path, me, errors_seen, dry_run=a.dry_run):
      print(l)
    return
  # Claim the pidfile before anything slow (Me() asks gh for the login), so `xms watch`
  # sees it promptly and a second watcher exits without a network call.
  try:
    old = int(open(pidf).read().strip())
    os.kill(old, 0)
    if "watch.py" in open(f"/proc/{old}/cmdline").read():
      sys.exit(f"watcher already running (pid {old})")
  except (OSError, ValueError):
    pass
  with open(pidf, "w") as f:
    f.write(str(os.getpid()))
  me = Me()
  if os.path.exists(stop_file):
    os.remove(stop_file)
  deadline = time.time() + a.max_hours * 3600
  print(f"watching {db_path} every {a.interval}s until {time.ctime(deadline)}; me={sorted(me.ids)}", flush=True)
  first_round, round_fails = True, 0
  try:
    while time.time() < deadline and not os.path.exists(stop_file):
      try:
        sweep = sweep_my_prs(db_path, log_disabled=first_round)
        if sweep:
          print(f"{now_iso()} " + "; ".join(sweep), flush=True)
      except Exception as e:  # pylint: disable=broad-except
        print(f"{now_iso()} PR sweep failed: {e}", file=sys.stderr, flush=True)
      first_round = False
      try:
        lines = check_round(db_path, me, errors_seen, fails)
        hw = collector_health(os.path.dirname(db_path), xmsconf.lang(hubdb.read(db_path)))
        if hw and not locals().get("health_warned"):
          lines.append("⚠️ " + hw)
          health_warned = True
        elif not hw:
          health_warned = False
        round_fails = 0
        print(f"{now_iso()} round ok, {len(lines)} notable", flush=True)
      except Exception as e:  # pylint: disable=broad-except
        round_fails += 1
        print(f"{now_iso()} round FAILED ({round_fails}x): {e}", file=sys.stderr, flush=True)
        lines = [f"⚠️ {round_fails} rounds in a row failed: {str(e)[:200]}"] if round_fails == 3 else []
      if lines:
        notify(a.notify, _text(db_path, "watch_head") + "\n" + "\n".join(lines))
      for _ in range(a.interval):
        if os.path.exists(stop_file):
          break
        time.sleep(1)
    if not os.path.exists(stop_file):
      notify(a.notify, _text(db_path, "watch_head") + _text(db_path, "stopped", hours=f"{a.max_hours:g}", cmd="watch"))
  finally:
    try:
      os.remove(pidf)
    except OSError:
      pass


if __name__ == "__main__":
  main()
