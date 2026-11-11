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

"""xiaomishu-work ledger: which events were already reported, shared by watcher + inbox collector.

$XMS_HOME/ledger.jsonl, append-only, one {key, by, at, ...meta} per line. Keys come from
event_key()/key_*() so one event seen via different channels (API, email, chat) dedups.

Key shapes:
  https://github.com/o/r/pull/7#issuecomment-1   GitHub comment, review, review comment (also other gh_hosts)
  https://github.com/o/r/pull/7#merged           (also #closed)
  chat:spaces/S/messages/M                       M = message id without the "<thread>." prefix
  cl:<num>:<comment id>  cl:<num>:submitted       cl:<num>:mail:<gmail id> (coarse)
  gmail:<message id>

Usage: ledger.py seen KEY...  |  ledger.py record KEY [--by X] [--item I] [--note N]  |  ledger.py tail [-n 20]
"""

import sys
sys.dont_write_bytecode = True

import argparse
import base64
import contextlib
import datetime as dt
import fcntl
import json
import os
import re
import urllib.parse

sys.path.insert(0, os.path.dirname(os.path.realpath(__file__)))
import hubdb  # noqa: E402
import xmsconf  # noqa: E402


def path():
  return os.environ.get("XMS_LEDGER") or os.path.join(hubdb.xms_home(), "ledger.jsonl")


@contextlib.contextmanager
def _locked(p):
  os.makedirs(os.path.dirname(p) or ".", exist_ok=True)
  with open(p + ".lock", "a") as lf:
    fcntl.flock(lf, fcntl.LOCK_EX)
    try:
      yield
    finally:
      fcntl.flock(lf, fcntl.LOCK_UN)


def entries(p=None):
  p = p or path()
  try:
    with open(p, encoding="utf-8") as f:
      lines = f.readlines()
  except FileNotFoundError:
    return []
  out = []
  for l in lines:
    try:
      out.append(json.loads(l))
    except ValueError:
      pass  # torn line from a crash; ignore
  return out


def canon(key):
  """Raw URLs (e.g. pasted by the triage run) -> canonical key; other keys unchanged."""
  return (key_url(key) or key) if key and re.match(r"https?://", key) else key


def seen(keys, ignore=None, p=None):
  """-> the subset of `keys` (as given) already recorded. `ignore(entry)` drops entries from consideration."""
  want = {}
  for k in keys:
    if k:
      want.setdefault(canon(k), set()).add(k)  # several inputs may share one canonical key
  if not want:
    return set()
  out = set()
  for e in entries(p):
    if e.get("key") in want and not (ignore and ignore(e)):
      out |= want[e["key"]]
  return out


def record(key, by, p=None, **meta):
  record_many([key], by, p=p, **meta)


def record_many(keys, by, p=None, **meta):
  p = p or path()
  at = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
  meta = {k: v for k, v in meta.items() if v not in (None, "", False)}
  rows = [json.dumps(dict(key=canon(k), by=by, at=at, **meta), ensure_ascii=False) for k in keys if k]
  if not rows:
    return
  with _locked(p):
    with open(p, "a", encoding="utf-8") as f:
      f.write("\n".join(rows) + "\n")


# ---- key canonicalization ----

def key_url(url):
  """GitHub-ish or Chat URL -> key, else None."""
  if not url:
    return None
  u = urllib.parse.urlsplit(url.strip())
  host = (u.hostname or "").lower()
  if host in xmsconf.gh_hosts():
    m = re.match(r"/([^/]+)/([^/]+)/(issues|pulls?)/(\d+)", u.path)
    if not m:
      return None
    kind = "issues" if m.group(3) == "issues" else "pull"
    base = f"https://{host}/{m.group(1).lower()}/{m.group(2).lower()}/{kind}/{m.group(4)}"
    return f"{base}#{u.fragment.lower()}" if u.fragment else base
  if host == "chat.google.com":
    parts = [x for x in u.path.split("/") if x]
    if len(parts) >= 3 and parts[0] in ("room", "dm"):
      return key_chat(parts[1], parts[-1])
  for pat in cl_url_patterns():
    m = re.match(pat, url)
    if m:
      return f"cl:{m.group(1)}"
  return None


def cl_url_patterns():
  """Regexes (group 1 = CL number) for CL links: from the tools.json cl_url template, plus
  any extra tools.json cl_url_patterns."""
  out = list(xmsconf.tool_setting("cl_url_patterns", []) or [])
  t = xmsconf.tool_setting("cl_url")
  if t and "{num}" in t:
    out.append(re.escape(t).replace(re.escape("{num}"), r"(\d+)"))
  return out


def gh_ref(url):
  """GitHub-ish URL or 'o/r#N' -> 'host/o/r#N' (source identity, for covered_by), else None."""
  m = re.match(r"https?://([^/]+)/([^/]+/[^/#?]+)/(?:issues|pulls?)/(\d+)", url or "")
  if m and m.group(1).lower() in xmsconf.gh_hosts():
    return f"{m.group(1).lower()}/{m.group(2).lower()}#{m.group(3)}"
  m = re.fullmatch(r"([^/#\s]+/[^/#\s]+)#(\d+)", url or "")
  return f"github.com/{m.group(1).lower()}#{m.group(2)}" if m else None


def key_chat(space_or_name, msg=None):
  """key_chat('spaces/S/messages/T.M') or key_chat('S', 'M' or 'T.M')."""
  if msg is None:
    m = re.match(r"spaces/([^/]+)/messages/(.+)", space_or_name or "")
    if not m:
      return None
    space_or_name, msg = m.groups()
  space = str(space_or_name).split("/")[-1]
  return f"chat:spaces/{space}/messages/{str(msg).split('.')[-1]}"


def key_cl(num, comment_id):
  return f"cl:{re.sub(r'[^0-9]', '', str(num))}:{comment_id}"


def event_key(e):
  """Watcher/collector event dict -> key: explicit `key`, else from `chat_name`, else `url`."""
  if e.get("key"):
    return e["key"]
  if e.get("chat_name"):
    return key_chat(e["chat_name"])
  return key_url(e.get("url"))


# ---- email ----

def headers(msg):
  return {(h.get("name") or "").lower(): h.get("value") or "" for h in (msg.get("payload") or {}).get("headers") or [] if h.get("name")}


def body_text(msg):
  """Gmail API message -> text/plain body (falls back to tag-stripped html, then snippet)."""
  found = {}

  def walk(p):
    mt, data = p.get("mimeType", ""), (p.get("body") or {}).get("data")
    if data and mt in ("text/plain", "text/html") and mt not in found:
      found[mt] = base64.urlsafe_b64decode(data + "==").decode("utf-8", "replace")
    for c in p.get("parts") or []:
      walk(c)
  walk(msg.get("payload") or {})
  if "text/plain" in found:
    return found["text/plain"]
  if "text/html" in found:
    return re.sub(r"<[^>]+>", " ", found["text/html"])
  return msg.get("snippet") or ""


def email_keys(msg, cl_comments=None):
  """Gmail API message -> (keys, coarse). keys[0] is primary; any key already seen means "reported".

  cl_comments(num) -> list of code-review comments ({id, author, publishTimestamp}) lets CL review
  mail map to the exact comment keys the watcher uses; without a match the key is coarse.
  """
  h, text, mid = headers(msg), body_text(msg), msg.get("id")
  if h.get("x-github-reason") or "github" in h.get("message-id", ""):
    m = re.search(r"<([^/]+/[^/]+)/(pull|issues)/(\d+)@", h.get("in-reply-to", "") + h.get("message-id", ""))
    st = re.search(r"^(Merged|Closed) #\d+", text, re.M)
    if m and st:
      # GitHub Enterprise hosts put their own hostname after the "@" in the Message-ID.
      dom = h.get("message-id", "").lower().rsplit("@", 1)[-1].rstrip(">")
      host = next((x for x in xmsconf.gh_hosts()[1:] if dom == x or dom.endswith("." + x)), "github.com")
      base = key_url(f"https://{host}/{m.group(1)}/{m.group(2)}/{m.group(3)}")
      return [f"{base}#{st.group(1).lower()}"], False
    m = re.search(r"view it on GitHub:?\s*<?(https?://\S+?#\S+?)>?\s", text + "\n")
    if not m:
      hosts = "|".join(re.escape(x) for x in xmsconf.gh_hosts())
      m = re.search(rf"(https?://(?:{hosts})/\S+?/(?:pull|issues)/\d+#\S+?)[\s>]", text + "\n")
    if m and key_url(m.group(1)):
      return [key_url(m.group(1))], False
  num = _review_cl(h, text)
  if num:
    if re.search(r"was submitted as \d+", text):
      return [f"cl:{num}:submitted"], False
    keys = _match_cl_comments(num, h, cl_comments) if cl_comments else []
    if keys:
      return keys, False
    return [f"cl:{num}:mail:{mid}"], True
  return [f"gmail:{mid}"], False


def _review_cl(h, text):
  """CL number of a code-review notification mail (recognized by its headers), else None."""
  if not (h.get("x-from-critique") or "@critique>" in h.get("message-id", "")):
    return None
  m = re.match(r"\s*(?:Re:\s*)?CL (\d+)", h.get("subject", "")) or re.search(r"#review/(\d+)", text)
  return m.group(1) if m else None


def _match_cl_comments(num, h, cl_comments):
  """Comments by the mail's sender published within 10 min before the mail's Date."""
  m = re.search(r"<([^@>]+)@", h.get("from", ""))
  author = m.group(1).lower() if m else ""
  try:
    sent = dt.datetime.strptime(re.sub(r"\s*\(.*\)$", "", h.get("date", "")), "%a, %d %b %Y %H:%M:%S %z").timestamp()
  except ValueError:
    return []
  try:
    comments = cl_comments(num) or []
  except Exception:  # pylint: disable=broad-except
    return []
  out = []
  for c in comments:
    ts = int(c.get("publishTimestamp") or c.get("timestamp") or 0)
    if (c.get("author") or "").lower() == author and -60 <= sent - ts <= 600:
      out.append(key_cl(num, c.get("id")))
  return out


def main(argv=None):
  ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
  sub = ap.add_subparsers(dest="cmd", required=True)
  s = sub.add_parser("seen")
  s.add_argument("keys", nargs="+")
  r = sub.add_parser("record")
  r.add_argument("key")
  r.add_argument("--by", default="manual")
  r.add_argument("--item")
  r.add_argument("--note")
  t = sub.add_parser("tail")
  t.add_argument("-n", type=int, default=20)
  k = sub.add_parser("key", help="print the canonical key for a URL")
  k.add_argument("url")
  a = ap.parse_args(argv)
  if a.cmd == "seen":
    got = seen(a.keys)
    for key in a.keys:
      print(f"{'seen' if key in got else 'new '} {key}")
    sys.exit(0 if got == set(a.keys) else 1)
  elif a.cmd == "record":
    record(a.key, a.by, item=a.item, note=a.note)
  elif a.cmd == "tail":
    for e in entries()[-a.n:]:
      print(json.dumps(e, ensure_ascii=False))
  elif a.cmd == "key":
    print(key_url(a.url) or "")


if __name__ == "__main__":
  main()
