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

"""xiaomishu-work inbox collector: gathers new Chat/Gmail activity into batches for the triage run.

OPTIONAL. It needs a Chat CLI ("chat") and/or a Gmail CLI ("mail"), configured with
$XMS_CHAT_CLI / $XMS_MAIL_CLI or in $XMS_HOME/tools.json (see references/inbox.md for the
commands and JSON shapes they must provide). Sources whose CLI isn't configured are skipped
with one log line; with none configured the collector exits.

Each round (per source, since that source's last successful round):
  mention   Chat @mentions of the owner                 (chat list-mentions; semantic-search fallback on quota errors)
  thread    replies in threads the owner posted in      (chat semantic-search --from me, then stream read-thread)
  dm        DMs / group chats with activity (+ prior context)  (list-spaces activity -> messages, via stream)
  space     newly joined spaces (first run = baseline)  (list-spaces diff)
  topic     topic sweep over all spaces                 (chat semantic-search; topics in $XMS_HOME/topics.txt)
  gmail     inbox mail (read or not) minus bulk/list traffic, plus GitHub mention/review/assign
            notifications and meeting notes even if already read   (mail readonly read)
Candidates whose ledger key is already recorded are dropped; the rest go to
$XMS_HOME/inbox/batch-<ts>.json and the triage run ($XMS_TRIAGE_RUN) gets a short summary.
Candidates on a source an open hub item watches get covered_by=<item>; their ledger entries
keep covered_by so the watcher still forwards them to the project run.

Usage: inbox_collect.py [--once] [--dry-run] [--max-hours 24] [--notify CMD]
  --dry-run   print the batch + message; no notify, no ledger/state writes (batch file still written).
Schedule ($XMS_HOME/inbox_config.json overrides DEFAULTS; tz "" = system local time): Mon-Fri
09:00-18:30 every 10 min, other weekday hours hourly, weekends off. Stop: $XMS_HOME/.inbox.stop.
Owner display name (for the search mention fallback): $XMS_OWNER_NAME > inbox_config "owner_name" >
learned from the owner's own Chat messages > $USER. Read-only on Chat and Gmail.
"""

import sys
sys.dont_write_bytecode = True

import argparse
import datetime as dt
import json
import math
import os
import re
import subprocess
import time
import zoneinfo

sys.path.insert(0, os.path.dirname(os.path.realpath(__file__)))
import hubdb  # noqa: E402
import ledger  # noqa: E402
import xmsconf  # noqa: E402
from xmsconf import load_json  # noqa: E402
from watch import CLOSED, Me, iso, notify, now_iso, parse_ts, run_json  # noqa: E402
from xms_tell import tell  # noqa: E402

SOURCE_TOOL = {"mention": "chat", "thread": "chat", "dm": "chat", "space": "chat", "topic": "chat", "gmail": "mail"}
DEFAULTS = {
    "tz": "", "work_days": [0, 1, 2, 3, 4], "work_start": "09:00", "work_end": "18:30",
    "work_every_min": 10, "off_every_min": 60, "weekends": False,
    "first_lookback_hours": 4, "max_lookback_hours": 72,
    "overlap_min": 20,  # re-scan this much before `since` (search-index lag); the ledger dedups
    "gmail_query": "in:inbox -category:promotions -category:social",  # read or not: opened != handled
    "gmail_skip_from": [],  # sender addresses to ignore, e.g. automated notices
    "notes_from": [],  # senders of meeting notes to keep even if already read
    "sources": ["mention", "thread", "dm", "space", "topic", "gmail"],
    "owner_name": "",
    "owner_email": "",  # default: $XMS_OWNER_EMAIL, else an address in $XMS_ME
    # Error text that means "credentials expired": one login hint instead of per-source alerts.
    "auth_error_patterns": ["non-JSON output", "unauthenticated", "UNAUTHENTICATED", "credentials"],
    "login_hint": "",  # e.g. the command that refreshes your credentials
}
TOPICS_TEMPLATE = """# Topics to sweep across all Chat spaces, one per line ('#' = comment).
# ' / ' separates alternative spellings that are searched separately. Examples:
# my-project design review
# Flaky test / flake / deflake
"""
CHANNEL_ORDER = ["dm", "mention", "thread", "gmail", "topic", "space"]
TEXT_MAX = 1500


def home():
  return hubdb.xms_home()


def hpath(*p):
  return os.path.join(home(), *p)


def save_json(p, data):
  tmp = p + ".tmp"
  with open(tmp, "w", encoding="utf-8") as f:
    json.dump(data, f, ensure_ascii=False, indent=1)
  os.replace(tmp, p)


def config():
  return {**DEFAULTS, **load_json(hpath("inbox_config.json"), {})}


def topics():
  p = hpath("topics.txt")
  if not os.path.exists(p):
    with open(p, "w", encoding="utf-8") as f:
      f.write(TOPICS_TEMPLATE)
  with open(p, encoding="utf-8") as f:
    lines = [l.strip() for l in f if l.strip() and not l.startswith("#")]
  return [(l, [x.strip() for x in l.split(" / ") if x.strip()]) for l in lines]


def every_minutes(cfg, local):
  """Minutes until the next round at local time `local`; 0 = off."""
  if local.weekday() not in cfg["work_days"]:
    return cfg["off_every_min"] if cfg["weekends"] else 0
  hm = local.strftime("%H:%M")
  return cfg["work_every_min"] if cfg["work_start"] <= hm < cfg["work_end"] else cfg["off_every_min"]


def clip(s, n=TEXT_MAX):
  s = (s or "").strip()
  return s if len(s) <= n else s[:n - 1] + "…"


def cand(channel, key, where, who, at, text, url, source=None, alt_keys=(), **extra):
  return dict(key=key, alt_keys=[k for k in alt_keys if k and k != key], channel=channel, where=where, who=who,
              at=iso(at) if isinstance(at, dt.datetime) else at, text=clip(text), url=url, source=source,
              **{k: v for k, v in extra.items() if v not in (None, "", [], False)})


# ---- Chat ----

def _sender(s):
  s = s or {}
  return s.get("email") or s.get("display_name") or s.get("name") or "?", s.get("display_name") or s.get("email")


def stream(sub, rows, *flags, timeout=300):
  """<chat CLI> readonly stream <sub>: rows of input JSON -> {id: data}; failed rows raise if all failed."""
  inp = "\n".join(json.dumps(r) for r in rows) + "\n"
  p = subprocess.run([xmsconf.need("chat"), "readonly", "stream", sub, "-i", "-", *flags], input=inp, capture_output=True,
                     text=True, timeout=timeout)
  out, errs = {}, []
  for l in p.stdout.splitlines():
    try:
      r = json.loads(l)
    except ValueError:
      continue
    if r.get("_ok"):
      out[r.get("id")] = r.get("data")
    else:
      errs.append(str(r.get("error") or r)[:160])
  if not out and (errs or p.returncode):
    raise RuntimeError(f"stream {sub}: " + (errs[0] if errs else (p.stderr.strip().splitlines() or ["failed"])[-1]))
  return out


def src_mention(since, me, ctx):
  hours = max(1, math.ceil((dt.datetime.now(dt.timezone.utc) - since).total_seconds() / 3600) + 1)
  try:
    rows = run_json([xmsconf.need("chat"), "readonly", "list-mentions", "--hours", str(hours), "--max", "100", "--json"], 300) or []
  except RuntimeError as e:
    if not any(k in str(e) for k in ("rateLimit", "429", "quota", "RESOURCE_EXHAUSTED")):
      raise
    # The Chat message-search quota is often exhausted; semantic search doesn't use it,
    # so fall back to it (no message ids -> keys are thread+time based).
    return _mentions_via_search(since, me, ctx)
  out = []
  for r in rows:
    m = r.get("message") or {}
    at = parse_ts(m.get("create_time"))
    who, name = _sender(m.get("sender"))
    if not at or at <= since or me(who):
      continue
    space = r.get("space_id") or ""
    where = r.get("display_name") if r.get("display_name") not in (None, "", space) else f"{r.get('space_type','')} {space}"
    out.append(cand("mention", ledger.key_chat(m.get("name", "")), where, name or who, at, m.get("text"), m.get("url"),
                    source=f"chat:{space}"))
  return out


def _from_me(ctx):
  """Semantic search for the owner's own recent messages (cached per round; also used by src_thread)."""
  if "from_me" not in ctx:
    ctx["from_me"] = run_json([xmsconf.need("chat"), "readonly", "semantic-search", "-q", "messages I sent", "--from", "me",
                               "--sort-by", "latest", "--datetime", json.dumps({"time_phrase": "last 3 days"})],
                              120) or {}
  return ctx["from_me"]


def owner_name(me, ctx):
  """Owner's Chat display name: $XMS_OWNER_NAME > config > learned (cached in state) > $USER."""
  name = os.environ.get("XMS_OWNER_NAME") or ctx["cfg"].get("owner_name") or ctx["state"].get("owner_name")
  if not name:
    try:
      txt = json.dumps(_from_me(ctx), ensure_ascii=False)
      for n, email in re.findall(r"Sender: ([^<\\]+?) <([^>]+)>", txt):
        if me(email):
          name = n.strip()
          ctx["new_state"]["owner_name"] = name
          break
    except Exception:  # pylint: disable=broad-except
      pass
  return name or os.environ.get("USER", "")


def mentions_owner(text, name, user):
  """Does a search-rendered message @ the owner? Chat renders mentions as '@<display name>'."""
  first = (name or "").split()[0] if (name or "").split() else ""
  pats = [rf"@{re.escape(first)}\b"] if first else []
  if user:
    pats.append(rf"\b{re.escape(user)}@")
  pats.append(r"@all\b")
  return bool(re.search("|".join(pats), text, re.I))


def _mentions_via_search(since, me, ctx):
  d = run_json([xmsconf.need("chat"), "readonly", "semantic-search", "-q", "mentions of me", "--mentions", "me",
                "--sort-by", "latest", "--datetime", json.dumps({"time_phrase": "last 3 days"})], 120) or {}
  out, seen = [], set()
  name, user = owner_name(me, ctx), os.environ.get("USER", "")
  for c in d.get("assembled_context") or []:
    ref = (c.get("$ref") or [""])[0]
    where = ref.split(" - http")[0].strip()
    m = re.search(r"chat\.google\.com/(?:room|dm)/([^/]+)/([^/\s]+)", ref)
    space, thread = (m.group(1), m.group(2)) if m else ("?", "?")
    txt = " ".join(x.get("text", "") for x in c.get("content") or [])
    for blk in re.split(r"\[(?:Read|Unread) Message\]", txt)[1:]:
      mm = re.search(r"Sender: (.*?) <(.*?)>\s*Time: (\S+)\s*Text: (.*)", blk, re.S)
      if not mm:
        continue
      name, email, ts, text = mm.groups()
      if not mentions_owner(text, name, user):
        continue
      at = parse_ts(ts)
      if not at or at <= since or me(email):
        continue
      key = f"chat:search:{space}/{thread}/{ts}"
      if key in seen:
        continue
      seen.add(key)
      out.append(cand("mention", key, where or space, name, at, text.strip(),
                      f"https://chat.google.com/room/{space}/{thread}", source=f"chat:{space}"))
  return out


def spaces(ctx):
  if "spaces" not in ctx:
    ctx["spaces"] = run_json([xmsconf.need("chat"), "readonly", "list-spaces", "--json"], 300)
  return ctx["spaces"]


def src_dm(since, me, ctx):
  active = [s for s in spaces(ctx) if s.get("space_type") in ("DIRECT_MESSAGE", "GROUP_CHAT")
            and (parse_ts(s.get("last_active_time")) or since) > since]
  if not active:
    return []
  # Read != handled: take every DM / group chat with activity since the last round, whether or
  # not the owner already opened it (a message read within the 10-minute window used to be skipped).
  # The ledger keeps items from being reported twice; triage sees context and skips what she answered.
  unread = [(s, since) for s in active]
  msgs = stream("list-messages", [{"space_id": s["name"]} for s, _ in unread], "--since", iso(since), "--max", "50")
  out = []
  for s, after in unread:
    for m in msgs.get(s["name"]) or []:
      at = parse_ts(m.get("create_time"))
      who, name = _sender(m.get("sender"))
      if not at or at <= after or me(who):
        continue
      kind = "DM" if s["space_type"] == "DIRECT_MESSAGE" else "group chat"
      where = f"{kind} {s.get('display_name') or ''}".strip() + (f" with {name}" if kind == "DM" else f" {s['name']}")
      out.append(cand("dm", ledger.key_chat(m.get("name", "")), where, name or who, at, m.get("text"),
                      m.get("url"), source=f"chat:{s['name'].split('/')[-1]}"))
  return out


def src_space(since, me, ctx):
  """Newly joined spaces/group chats, diffed against state['spaces'] (first run: baseline only)."""
  cur = {s["name"]: s for s in spaces(ctx) if s.get("space_type") in ("SPACE", "GROUP_CHAT")}
  known = ctx["state"].get("spaces")
  ctx["new_state"]["spaces"] = sorted(cur)
  if known is None:
    return []
  out = []
  for name in sorted(set(cur) - set(known)):
    s = cur[name]
    desc = (s.get("space_details") or {}).get("description") or ""
    out.append(cand("space", f"space:{name}", s.get("display_name") or name, "", s.get("create_time") or now_iso(),
                    f"joined {s.get('space_type','').lower()} ({s.get('membership_count','?')} members) {desc}",
                    (s.get("space_uri") or "").split("?")[0], source=f"chat:{name.split('/')[-1]}"))
  return out


_SEARCH_MSG = re.compile(r"\[(?:Unread|Read) Message\]\nSender: (.*?)\nTime: (\S+)\nText: (.*?)"
                       r"(?=\n\n\[(?:Unread|Read) Message\]|\n\nReplies: \[|\n\]\n|\Z)", re.S)


def search_messages(chunk):
  """Semantic-search chat chunk -> [(key, who, at, text, url, unread)]; ids zip with chat_message_data in order."""
  md = (chunk.get("metadata") or {}).get("chat_space_data") or {}
  space, thread = md.get("space_id"), md.get("thread_id")
  text = "".join(c.get("text") or "" for c in chunk.get("content") or [])
  found = list(_SEARCH_MSG.finditer(text))
  ids = [x.get("message_id") for x in md.get("chat_message_data") or []]
  out = []
  for i, m in enumerate(found):
    mid = ids[i] if len(ids) == len(found) else None
    key = ledger.key_chat(space, mid) if mid else f"chat:spaces/{space}/threads/{thread}@{m.group(2)}"
    url = f"https://chat.google.com/room/{space}/{thread}/{mid or thread}"
    out.append((key, m.group(1).strip(), parse_ts(m.group(2)), m.group(3).strip(), url,
                m.group(0).startswith("[Unread")))
  return md.get("title") or space, space, out


def src_topic(since, me, ctx):
  out, by_key, errs = [], {}, []
  for label, queries in topics():
    for q in queries:
      try:
        d = run_json([xmsconf.need("chat"), "readonly", "semantic-search", "-q", q, "--mandatory-keywords", q,
                      "--sort-by", "latest", "--quiet-thoughts", "--json"], 300)
      except RuntimeError as e:
        errs.append(f"{q}: {e}")
        continue
      for chunk in d.get("assembled_context") or []:
        title, space, msgs = search_messages(chunk)
        root_hit = bool(msgs) and mentions_all(msgs[0][3], q)
        for i, (key, who, at, text, url, unread) in enumerate(msgs):
          if not at or at <= since or me(_email(who)):
            continue
          if not (mentions_all(text, q) or (i > 0 and root_hit)):
            continue  # search returns whole threads; keep on-topic messages and replies to on-topic roots
          if key in by_key:
            by_key[key].setdefault("topics", [])
            if label not in by_key[key]["topics"]:
              by_key[key]["topics"].append(label)
            continue
          c = cand("topic", key, title, who, at, text, url, source=f"chat:{space}", topics=[label], unread=unread)
          by_key[key] = c
          out.append(c)
  if errs and not out and len(errs) == sum(len(q) for _, q in topics()):
    raise RuntimeError(errs[0])
  return out


def mentions_all(text, query):
  t = (text or "").lower()
  return all(w in t for w in query.lower().split())


def _email(who):
  m = re.search(r"<([^>]+)>", who or "")
  return m.group(1) if m else who


# ---- Gmail ----

def _addr_in(owner, *fields):
  return bool(owner) and any(owner.lower() in (f or "").lower() for f in fields)


def keep_mail(msg, owner):
  """Drops bulk/list traffic not addressed to the owner (GitHub notifications are kept)."""
  labels, h = set(msg.get("labelIds") or []), ledger.headers(msg)
  if labels & {"CATEGORY_PROMOTIONS", "CATEGORY_SOCIAL", "SPAM", "TRASH"}:
    return False
  if h.get("x-github-reason"):
    return True
  direct = _addr_in(owner, h.get("to"), h.get("cc"))
  if (h.get("list-id") or h.get("x-beenthere")) and not direct:
    return False
  if h.get("precedence", "").lower() in ("bulk", "list", "junk") and not direct:
    return False
  return True


def _mail_text(msg):
  t = ledger.body_text(msg)
  t = re.split(r"\n-- ?\n|\nOn .{10,80} wrote:\n", t, maxsplit=1)[0]
  return re.sub(r"\n{3,}", "\n\n", t)


def src_gmail(since, me, ctx):
  cfg, owner = ctx["cfg"], ctx["owner"]
  q = f"{cfg['gmail_query']} after:{int(since.timestamp())}"
  mail = xmsconf.need("mail")
  msgs = run_json([mail, "readonly", "read", q, "--max", "50", "--json"], 300) or []
  # Read != handled: GitHub notifications that @mention / request review from / assign the owner,
  # and meeting notes from cfg["notes_from"], are collected even if already opened (the ledger
  # keeps them from being reported twice).
  seen_ids = {m.get("id") for m in msgs}
  notes_from = [x.lower() for x in cfg.get("notes_from") or []]
  froms = " OR ".join(f"from:{x}" for x in ["notifications@github.com", *notes_from])
  gq = f"({froms}) after:{int(since.timestamp())}"
  for m in run_json([mail, "readonly", "read", gq, "--max", "50", "--json"], 300) or []:
    if m.get("id") in seen_ids:
      continue
    h = ledger.headers(m)
    reason = h.get("x-github-reason", "")
    # GitHub @mention / review / assign, and meeting notes: opened != handled.
    if reason in ("mention", "team_mention", "review_requested", "assign") or _email(h.get("from", "")).lower() in notes_from:
      msgs.append(m)
      seen_ids.add(m.get("id"))
  cache, cl_cli = {}, xmsconf.tool("cl")

  def cl_comments(num):
    if not cl_cli:
      return []  # no code-review CLI: CL mail keeps a coarse key
    if num not in cache:
      cache[num] = run_json([cl_cli, "readonly", "list-comments", f"--cl={num}", "--json", "--limit=200"])
    return cache[num]

  out = []
  for m in msgs:
    at = dt.datetime.fromtimestamp(int(m.get("internalDate", "0")) / 1000, dt.timezone.utc)
    h = ledger.headers(m)
    sender = _email(h.get("from", "")).lower()
    if at <= since or not keep_mail(m, owner) or me(sender) or sender in cfg["gmail_skip_from"]:
      continue
    keys, coarse = ledger.email_keys(m, cl_comments)
    source = None
    if keys[0].startswith("https://"):
      source = ledger.gh_ref(keys[0])
    elif keys[0].startswith("cl:"):
      source = "cl:" + keys[0].split(":")[1]
    out.append(cand("gmail", keys[0], h.get("subject", ""), h.get("from", ""), at, _mail_text(m),
                    f"https://mail.google.com/mail/u/0/#all/{m.get('threadId')}", source=source,
                    alt_keys=keys[1:] + [f"gmail:{m.get('id')}"], coarse=coarse))
  return out


def src_thread(since, me, ctx):
  """Replies from others in threads the owner posted in recently (no @mention needed).
  Finds the threads via semantic search (cheap, no read quota), then reads them via `stream read-thread`
  (backs off on 429). DM threads are included too (the dm source can miss replies when Chat space quota runs out; the ledger dedups)."""
  d = _from_me(ctx)
  seen_t, rows = set(), []
  for c in d.get("assembled_context") or []:
    for ref in c.get("$ref") or []:
      m = re.search(r"chat\.google\.com/(?:room|dm)/([^/]+)/([^/\s]+)", ref)
      if m and m.groups() not in seen_t:
        seen_t.add(m.groups())
        title = ref.split(" - http")[0].strip()
        rows.append({"space_id": m.group(1), "thread_id": m.group(2), "_title": title})
  rows = rows[:25]
  if not rows:
    return []
  data = stream("read-thread", [{"space_id": r["space_id"], "thread_id": r["thread_id"]} for r in rows])
  titles = {r["space_id"]: r["_title"] for r in rows}
  out = []
  for msgs in data.values():
    for m in msgs or []:
      at = parse_ts(m.get("create_time"))
      who, name = _sender(m.get("sender"))
      if not at or at <= since or me(who):
        continue
      space = (m.get("name") or "").split("/")[1] if (m.get("name") or "").startswith("spaces/") else ""
      out.append(cand("thread", ledger.key_chat(m.get("name", "")), f"{titles.get(space, space)} (thread you posted in)",
                      name or who, at, m.get("text"), m.get("url"), source=f"chat:{space}"))
  return out


SOURCES = {"thread": src_thread, "mention": src_mention, "dm": src_dm, "space": src_space, "topic": src_topic, "gmail": src_gmail}


# ---- round ----

def watched_sources(db_path):
  """source id ('github.com/o/r#N', 'cl:N', 'chat:SPACE') -> open item id that watches it."""
  out = {}
  for iid, it in hubdb.read(db_path)["collections"].get("items", {}).items():
    w = it.get("watch")
    if not isinstance(w, dict) or it.get("status") in CLOSED:
      continue
    ref = str(w.get("ref") or "")
    if w.get("kind") in ("gh_pr", "gh_issue"):
      src = ledger.gh_ref(ref)
    elif w.get("kind") == "cl":
      src = "cl:" + re.sub(r"\D", "", ref)
    elif w.get("kind") == "chat_thread":
      src = "chat:" + ref.split("/")[-1]
    else:
      src = None
    if src:
      out[src] = iid
  return out


def available_sources(cfg):
  """Configured sources whose CLI is installed."""
  return [n for n in cfg["sources"] if n in SOURCES and xmsconf.tool(SOURCE_TOOL[n])]


def collect(state, cfg, me, owner, db_path):
  """-> (batch candidates, new_state, errors {source: msg}). Does not write anything."""
  now = dt.datetime.now(dt.timezone.utc)
  floor = now - dt.timedelta(hours=cfg["max_lookback_hours"])
  first = now - dt.timedelta(hours=cfg["first_lookback_hours"])
  new_state = json.loads(json.dumps(state))
  new_state.setdefault("since", {})
  ctx = {"state": state, "new_state": new_state, "cfg": cfg, "owner": owner}
  found, errors = [], {}
  for name in cfg["sources"]:
    since = max(parse_ts(state.get("since", {}).get(name)) or first, floor)
    since -= dt.timedelta(minutes=cfg["overlap_min"])
    try:
      got = SOURCES[name](since, me, ctx)
    except Exception as e:  # pylint: disable=broad-except
      errors[name] = str(e)[:200]
      continue
    new_state["since"][name] = iso(now)
    found += got
  seen_keys, uniq = set(), []
  for c in found:
    if c["key"] not in seen_keys:
      seen_keys.add(c["key"])
      uniq.append(c)
  done = ledger.seen([k for c in uniq for k in [c["key"], *c["alt_keys"]]])
  watched = watched_sources(db_path)
  batch = []
  for c in uniq:
    if c["key"] in done or any(k in done for k in c["alt_keys"]):
      continue
    if watched.get(c.get("source")):
      c["covered_by"] = watched[c["source"]]
    batch.append(c)
  batch.sort(key=lambda c: c.get("at") or "", reverse=True)
  batch.sort(key=lambda c: CHANNEL_ORDER.index(c["channel"]))  # stable: channel priority, newest first
  return batch, new_state, errors


def one_liner(c):
  text = re.sub(r"\s+", " ", c.get("text") or "")[:90]
  cov = f" [watched: {c['covered_by']}]" if c.get("covered_by") else ""
  return f"- [{c['channel']}] {c.get('where','')[:40]} — {c.get('who','')[:30]}: {text}{cov}"


def add_context(batch, me, max_spaces=6, per_item=6):
  """Attach the preceding messages of the same chat space (incl. the owner's own, from_owner=True),
  so triage can tell e.g. a reply to the owner's request from idle chit-chat. Best effort."""
  spaces = []
  for c in batch:
    m = re.match(r"chat:spaces/([^/]+)/messages/", c.get("key") or "")
    if m and m.group(1) not in spaces:
      spaces.append(m.group(1))
  spaces = spaces[:max_spaces]
  if not spaces:
    return
  try:
    data = stream("list-messages", [{"space_id": sp} for sp in spaces], "--window", "2d")
  except Exception as e:  # pylint: disable=broad-except
    log(f"context fetch failed: {e}")
    return
  by_space = {}
  for rid, msgs in data.items():
    for m in msgs or []:
      sp = (m.get("name") or "").split("/")[1] if (m.get("name") or "").startswith("spaces/") else None
      if sp:
        by_space.setdefault(sp, []).append(m)
  for c in batch:
    m = re.match(r"chat:spaces/([^/]+)/messages/", c.get("key") or "")
    if not m or m.group(1) not in by_space:
      continue
    at = parse_ts(c.get("at"))
    prior = sorted((x for x in by_space[m.group(1)] if (parse_ts(x.get("create_time")) or at) < at),
                   key=lambda x: x.get("create_time") or "")[-per_item:]
    ctx_rows = []
    for x in prior:
      who, name = _sender(x.get("sender"))
      ctx_rows.append({"who": name or who, "from_owner": bool(me(who)), "at": x.get("create_time", "")[:16],
                       "text": clip(x.get("text"), 300)})
    if ctx_rows:
      c["context"] = ctx_rows


def summary(batch, path, lang="en"):
  counts = {}
  for c in batch:
    counts[c["channel"]] = counts.get(c["channel"], 0) + 1
  head = ", ".join(f"{ch} {counts[ch]}" for ch in CHANNEL_ORDER if ch in counts)
  lines = [f"{xmsconf.text(lang, 'inbox_head')} {len(batch)} new: {head}", f"batch: {path}"]
  lines += [one_liner(c) for c in batch[:8]]
  if len(batch) > 8:
    lines.append(f"… +{len(batch) - 8} more in the batch file")
  return "\n".join(lines)


def do_round(a, cfg, me, owner, db_path):
  state_p = hpath("inbox_state.json")
  state = load_json(state_p, {})
  batch, new_state, errors = collect(state, cfg, me, owner, db_path)
  if batch:
    add_context(batch, me)
  fails = dict(state.get("fails") or {})
  alerts = []
  for name in cfg["sources"]:
    if name in errors:
      fails[name] = fails.get(name, 0) + 1
      log(f"source {name} failed ({fails[name]}x): {errors[name]}")
      if fails[name] == 3:
        alerts.append(f"⚠️ inbox source {name} failed 3 rounds in a row: {errors[name]}")
    else:
      fails.pop(name, None)
  new_state["fails"] = fails
  new_state["last_round"] = now_iso()
  path = None
  if batch:
    os.makedirs(hpath("inbox"), exist_ok=True)
    path = hpath("inbox", f"batch-{dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%SZ')}.json")
    save_json(path, {"created": now_iso(), "since": state.get("since", {}), "errors": errors, "items": batch})
  lang = xmsconf.lang(hubdb.read(db_path))
  msg = summary(batch, path, lang) if batch else None
  if a.dry_run:
    print(f"[dry-run] errors={errors}")
    print(msg or "[dry-run] empty batch -> no notification")
    for al in alerts:
      print(f"[dry-run] would alert: {al}")
    return batch
  for c in batch:
    ledger.record_many([c["key"], *c["alt_keys"]], "inbox", covered_by=c.get("covered_by"),
                       note=os.path.basename(path))
  save_json(state_p, new_state)
  if msg:
    run_id = os.environ.get("XMS_TRIAGE_RUN")
    err = tell(run_id, msg) if run_id else "XMS_TRIAGE_RUN not set"
    if err:
      log(f"triage notify failed: {err}")
      if not run_id:
        print(msg, flush=True)
  if alerts:
    blob = " ".join(alerts)
    if any(k in blob for k in cfg.get("auth_error_patterns") or []):
      srcs = ", ".join(sorted({al.split("inbox source ")[1].split(" ")[0] for al in alerts if "inbox source " in al}))
      hint = xmsconf.text(lang, "auth_hint", cmd=cfg["login_hint"]) if cfg.get("login_hint") else ""
      notify(a.notify, xmsconf.text(lang, "inbox_head") + xmsconf.text(lang, "auth", srcs=srcs, hint=hint))
    else:
      notify(a.notify, xmsconf.text(lang, "inbox_head") + "\n" + "\n".join(alerts))
  log(f"round ok: {len(batch)} new, errors={sorted(errors)}")
  return batch


def log(s):
  print(f"{now_iso()} {s}", flush=True)


def main(argv=None):
  ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
  ap.add_argument("--once", action="store_true")
  ap.add_argument("--dry-run", action="store_true")
  ap.add_argument("--max-hours", type=float, default=24)
  ap.add_argument("--notify", default=os.environ.get("XMS_NOTIFY", ""))
  ap.add_argument("--db", default=None)
  a = ap.parse_args(argv)
  if a.dry_run and not a.once:
    ap.error("--dry-run needs --once")
  db_path = hubdb.db_path(a.db)
  os.environ.setdefault("XMS_LEDGER", os.path.join(os.path.dirname(db_path), "ledger.jsonl"))
  cfg, me = config(), Me()
  owner = (os.environ.get("XMS_OWNER_EMAIL") or cfg.get("owner_email")
           or next((i for i in sorted(me.ids) if "@" in i), ""))
  avail = available_sources(cfg)
  for name in cfg["sources"]:
    if name not in avail:
      log(f"source {name} skipped: {xmsconf.ToolMissing(SOURCE_TOOL[name])}")
  if not avail:
    sys.exit("no inbox source available (configure a chat and/or mail CLI, see references/inbox.md); exiting")
  cfg["sources"] = avail
  if a.once:
    do_round(a, cfg, me, owner, db_path)
    return
  pidf, stop_file = hpath(".inbox.pid"), hpath(".inbox.stop")
  try:
    old = int(open(pidf).read().strip())
    os.kill(old, 0)
    if "inbox_collect.py" in open(f"/proc/{old}/cmdline").read():
      sys.exit(f"collector already running (pid {old})")
  except (OSError, ValueError):
    pass
  with open(pidf, "w") as f:
    f.write(str(os.getpid()))
  if os.path.exists(stop_file):
    os.remove(stop_file)
  deadline = time.time() + a.max_hours * 3600
  tz = zoneinfo.ZoneInfo(cfg["tz"]) if cfg["tz"] else None
  log(f"collecting into {home()} until {time.ctime(deadline)}; owner={owner}; triage={os.environ.get('XMS_TRIAGE_RUN')}")
  next_due = 0.0
  try:
    while time.time() < deadline and not os.path.exists(stop_file):
      if time.time() >= next_due:
        every = every_minutes(cfg, dt.datetime.now(tz) if tz else dt.datetime.now().astimezone())
        if every:
          try:
            do_round(a, cfg, me, owner, db_path)
          except Exception as e:  # pylint: disable=broad-except
            log(f"round failed: {e}")
          next_due = time.time() + every * 60
        else:
          next_due = time.time() + 600  # off hours: re-check the schedule every 10 min
      time.sleep(2)
    if not os.path.exists(stop_file):
      lang = xmsconf.lang(hubdb.read(db_path))
      notify(a.notify, xmsconf.text(lang, "inbox_head") + xmsconf.text(lang, "stopped", hours=f"{a.max_hours:g}", cmd="inbox"))
  finally:
    try:
      os.remove(pidf)
    except OSError:
      pass


if __name__ == "__main__":
  main()
