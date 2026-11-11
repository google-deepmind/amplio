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

"""Tests for ledger keys, watcher dedup and owner config. Run: python3 test_ledger.py (no network, temp dirs only)."""

import sys
sys.dont_write_bytecode = True

import base64
import datetime as dt
import json
import os
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.realpath(__file__)))
os.environ["XMS_HOME"] = tempfile.mkdtemp(prefix="xms-ledger-test-")
os.environ.pop("XMS_LEDGER", None)
os.environ.pop("XMS_DB", None)
os.environ["XMS_GH_HOSTS"] = "git.example.com"  # a second GitHub host (GitHub Enterprise style)
for _k in ("XMS_GH_CLI", "XMS_GHE_CLI", "XMS_CHAT_CLI", "XMS_MAIL_CLI", "XMS_CL_CLI", "XMS_GROUP_CLI"):
  os.environ.pop(_k, None)
import ledger  # noqa: E402
import watch as _watch  # noqa: E402

REAL_FETCH = _watch.fetch  # WatcherDedupTest replaces watch.fetch


def gmail_msg(mid, hdrs, body):
  data = base64.urlsafe_b64encode(body.encode()).decode().rstrip("=")
  return {"id": mid, "payload": {"mimeType": "multipart/alternative",
                                 "headers": [{"name": k, "value": v} for k, v in hdrs.items()],
                                 "parts": [{"mimeType": "text/plain", "body": {"data": data}}]}}


GH_REVIEW_MAIL = gmail_msg("fakemsg0001", {
    "From": "Alex Reviewer <notifications@github.com>",
    "Subject": "Re: [example-org/example-repo] fix: handle empty input (PR #123)",
    "Message-ID": "<example-org/example-repo/pull/123/review/1000001@github.com>",
    "In-Reply-To": "<example-org/example-repo/pull/123@github.com>",
    "X-GitHub-Reason": "author"},
    "@alex-reviewer approved this pull request.\n\nLGTM\n\n-- \nReply to this email directly or view it on GitHub:\n"
    "https://github.com/example-org/example-repo/pull/123#pullrequestreview-1000001\n"
    "You are receiving this because you authored the thread.\n")


class KeyTest(unittest.TestCase):

  def test_github_email_matches_api_review(self):
    api_html_url = "https://github.com/example-org/example-repo/pull/123#pullrequestreview-1000001"
    keys, coarse = ledger.email_keys(GH_REVIEW_MAIL)
    self.assertEqual(keys, [ledger.key_url(api_html_url)])
    self.assertFalse(coarse)

  def test_github_comment_variants(self):
    k = ledger.key_url("https://GitHub.com/Example-Org/example-repo/pull/123?notif=1#issuecomment-99")
    self.assertEqual(k, "https://github.com/example-org/example-repo/pull/123#issuecomment-99")
    self.assertEqual(ledger.key_url("https://git.example.com/example/repo/pulls/45#discussion_r5"),
                     "https://git.example.com/example/repo/pull/45#discussion_r5")
    self.assertIsNone(ledger.key_url("https://other.example.com/example/repo/pull/45"))

  def test_github_merged_email(self):
    msg = gmail_msg("m1", {"From": "x <notifications@github.com>", "X-GitHub-Reason": "author",
                           "Message-ID": "<example-org/example-repo/pull/123/issue_event/1@github.com>",
                           "In-Reply-To": "<example-org/example-repo/pull/123@github.com>"},
                    "Merged #123 into main.\n\n-- \nReply to this email directly or view it on GitHub:\n"
                    "https://github.com/example-org/example-repo/pull/123#event-123\n")
    self.assertEqual(ledger.email_keys(msg)[0], ["https://github.com/example-org/example-repo/pull/123#merged"])
    ghe = json.loads(json.dumps(msg).replace("@github.com>", "@git.example.com>"))
    self.assertEqual(ledger.email_keys(ghe)[0], ["https://git.example.com/example-org/example-repo/pull/123#merged"])

  def test_chat_name_and_url_agree(self):
    a = ledger.key_chat("spaces/SPACE000001/messages/THREAD00001.MSG00000001")
    b = ledger.key_url("https://chat.google.com/room/SPACE000001/THREAD00001/MSG00000001")
    c = ledger.key_chat("SPACE000001", "MSG00000001")
    self.assertEqual(a, "chat:spaces/SPACE000001/messages/MSG00000001")
    self.assertEqual({a, b, c}, {a})

  def test_cl_review_mail(self):
    date = "Thu, 29 Oct 2026 02:37:22 +0000"
    ts = int(dt.datetime(2026, 10, 29, 2, 37, 0, tzinfo=dt.timezone.utc).timestamp())
    msg = gmail_msg("g1", {"From": "Pat Example <patexample@example.com>", "Subject": "CL 100000001 (S) Example change",
                           "x-from-critique": "Yes", "Date": date}, "Some comment")
    comments = [{"id": "<c1@critique>", "author": "patexample", "publishTimestamp": str(ts)},
                {"id": "<c2@critique>", "author": "other", "publishTimestamp": str(ts)}]
    self.assertEqual(ledger.email_keys(msg, lambda n: comments), (["cl:100000001:<c1@critique>"], False))
    self.assertEqual(ledger.email_keys(msg), (["cl:100000001:mail:g1"], True))

  def test_plain_gmail(self):
    self.assertEqual(ledger.email_keys(gmail_msg("zz", {"From": "a@b"}, "hi")), (["gmail:zz"], False))

  def test_record_seen(self):
    p = os.path.join(os.environ["XMS_HOME"], "l1.jsonl")
    ledger.record("k1", "watcher", p=p, item="i")
    ledger.record("k2", "inbox", p=p, covered_by="i")
    self.assertEqual(ledger.seen(["k1", "k2", "k3"], p=p), {"k1", "k2"})
    self.assertEqual(ledger.seen(["k1", "k2"], ignore=lambda e: e.get("covered_by"), p=p), {"k1"})


  def test_seen_returns_every_alias(self):
    p = os.path.join(os.environ["XMS_HOME"], "l2.jsonl")
    ledger.record("https://github.com/o/r/pull/7#issuecomment-1", "watcher", p=p)
    a = "https://github.com/o/r/pull/7#issuecomment-1"
    b = "https://GitHub.com/O/R/pull/7?notif=1#issuecomment-1"  # same canonical key
    self.assertEqual(ledger.seen([a, b, "k-other"], p=p), {a, b})


class GithubSourceTest(unittest.TestCase):

  def run_src(self, pr, issue_extra=None, comments=None):
    def fake(cmd, timeout=90):
      path = cmd[-1]
      if path.endswith("/comments?per_page=100&since=2026-01-01T00:00:00Z"):
        return comments if "/issues/" in path and comments else []
      if path.endswith("/reviews?per_page=100"):
        return []
      if "/pulls/" in path:
        return pr
      return {"pull_request": {}, **(issue_extra or {})}
    orig, _watch.run_json = _watch.run_json, fake
    os.environ["XMS_GH_CLI"] = "sh"
    try:
      return _watch.src_github("o/r#7", dt.datetime(2026, 1, 1, tzinfo=dt.timezone.utc), True)
    finally:
      _watch.run_json = orig
      del os.environ["XMS_GH_CLI"]

  def test_closed_unmerged_pr(self):
    ev = self.run_src({"closed_at": "2026-02-01T00:00:00Z", "html_url": "https://github.com/o/r/pull/7"},
                      {"closed_by": None})
    self.assertEqual([(e["text"], e["key"]) for e in ev], [("[closed]", "https://github.com/o/r/pull/7#closed")])

  def test_merged_pr_is_not_also_closed(self):
    ev = self.run_src({"merged_at": "2026-02-01T00:00:00Z", "closed_at": "2026-02-01T00:00:00Z",
                       "merged_by": {"login": "m"}, "html_url": "https://github.com/o/r/pull/7"})
    self.assertEqual([e["text"] for e in ev], ["[merged]"])

  def test_deleted_user(self):
    ev = self.run_src({}, comments=[{"user": None, "created_at": "2026-02-01T00:00:00Z", "body": "x"}])
    self.assertEqual(ev[0]["who"], "ghost")


class WatcherDedupTest(unittest.TestCase):

  def setUp(self):
    import hubdb  # pylint: disable=g-import-not-at-top
    import watch  # pylint: disable=g-import-not-at-top
    self.watch, self.home = watch, tempfile.mkdtemp(prefix="xms-watch-test-")
    self.db = os.path.join(self.home, "hub.json")
    os.environ["XMS_LEDGER"] = os.path.join(self.home, "ledger.jsonl")
    seen = "2026-10-01T00:00:00Z"
    db = hubdb.empty_db()
    db["collections"]["projects"] = {"p": {"title": "P", "run_id": "run1"}}
    db["collections"]["items"] = {"i1": {"title": "PR", "project": "p", "status": "waiting_others",
                                          "watch": {"kind": "gh_pr", "ref": "o/r#1", "seen": seen}}}
    with open(self.db, "w") as f:
      json.dump(db, f)
    self.events, self.forwarded = [], []
    watch.fetch = lambda w, since: list(self.events)
    watch.forward = lambda run_id, iid, it, ev, **kw: self.forwarded.append([e["url"] for e in ev])
    self.me = lambda who: False

  def ev(self, n, day):
    return dict(who="bob", at=dt.datetime(2026, 10, day, tzinfo=dt.timezone.utc), text=f"c{n}",
                url=f"https://github.com/o/r/pull/1#issuecomment-{n}")

  def test_dedup(self):
    w = self.watch
    self.events = [self.ev(1, 2), self.ev(2, 3)]
    self.assertEqual(len(w.check_round(self.db, self.me, set())), 1)
    self.assertEqual(self.forwarded, [[e["url"] for e in self.events]])
    # Event 3 was already reported by the inbox collector -> item updated, no notify/forward.
    ledger.record("https://github.com/o/r/pull/1#issuecomment-3", "inbox")
    self.events.append(self.ev(3, 4))
    self.assertEqual(w.check_round(self.db, self.me, set()), [])
    self.assertEqual(len(self.forwarded), 1)
    import hubdb  # pylint: disable=g-import-not-at-top
    it = hubdb.read(self.db)["collections"]["items"]["i1"]
    self.assertEqual(it["watch"]["latest"]["text"], "c3")
    self.assertTrue(it["news"])
    # Event 4 was deferred by the inbox (covered_by) and 5 is unseen -> both forwarded.
    ledger.record("https://github.com/o/r/pull/1#issuecomment-4", "inbox", covered_by="i1")
    self.events += [self.ev(4, 5), self.ev(5, 6)]
    self.assertEqual(len(w.check_round(self.db, self.me, set())), 1)
    self.assertEqual(self.forwarded[-1], [self.ev(4, 5)["url"], self.ev(5, 6)["url"]])
    # Nothing new -> silent.
    self.assertEqual(w.check_round(self.db, self.me, set()), [])

  def test_list_watch_flagged_once_not_fatal(self):
    import hubdb  # pylint: disable=g-import-not-at-top
    hubdb.run_op(self.db, "add", "items/bad", {"title": "B", "status": "todo",
                                                "watch": [{"kind": "gh_pr", "ref": "o/r#2"}, {"kind": "cl"}]})
    hubdb.run_op(self.db, "add", "items/one", {"title": "O", "status": "todo",
                                                "watch": [{"kind": "gh_pr", "ref": "o/r#3"}]})
    self.events = [self.ev(1, 2)]
    errs = set()
    lines = self.watch.check_round(self.db, self.me, errs)
    self.assertEqual(len(lines), 2)  # the warning + the good item's news ("one" gets its baseline)
    self.assertIn("items/bad", lines[0])
    self.assertEqual(self.watch.check_round(self.db, self.me, errs), [])
    # A one-element list is accepted and rewritten as the plain object (with its baseline).
    self.assertEqual(hubdb.read(self.db)["collections"]["items"]["one"]["watch"]["ref"], "o/r#3")
    os.environ["XMS_SWEEP_REPOS"] = "o/"
    orig, self.watch.run_json = self.watch.run_json, lambda *a, **k: []
    try:
      self.assertEqual(self.watch.sweep_my_prs(self.db), [])  # list watch doesn't crash the sweep
    finally:
      self.watch.run_json = orig
      del os.environ["XMS_SWEEP_REPOS"]

  def test_dry_run_writes_nothing(self):
    self.events = [self.ev(1, 2)]
    def read():
      with open(self.db, encoding="utf-8") as f:
        return f.read()
    before = read()
    self.assertEqual(len(self.watch.check_round(self.db, self.me, set(), dry_run=True)), 1)
    self.assertEqual(read(), before)
    self.assertEqual(self.forwarded, [])
    self.assertFalse(os.path.exists(os.environ["XMS_LEDGER"]))


class OwnerConfigTest(unittest.TestCase):

  def setUp(self):
    import xmsconf  # pylint: disable=g-import-not-at-top
    self.xmsconf, self.home = xmsconf, tempfile.mkdtemp(prefix="xms-conf-test-")
    os.environ.pop("XMS_SWEEP_REPOS", None)
    os.environ.pop("XMS_LANG", None)

  def test_sweep_repos_default_off(self):
    import watch  # pylint: disable=g-import-not-at-top
    self.assertEqual(self.xmsconf.sweep_repos(self.home), [])
    called = []
    orig, watch.run_json = watch.run_json, lambda *a, **k: called.append(a) or []
    try:
      self.assertEqual(watch.sweep_my_prs(os.path.join(self.home, "hub.json"), log_disabled=False), [])
    finally:
      watch.run_json = orig
    self.assertEqual(called, [])  # disabled: never even asks gh

  def test_sweep_repos_config_and_env(self):
    with open(os.path.join(self.home, "watch_config.json"), "w") as f:
      json.dump({"sweep_repos": ["Example-Org/"]}, f)
    self.assertEqual(self.xmsconf.sweep_repos(self.home), ["example-org/"])
    os.environ["XMS_SWEEP_REPOS"] = "a/, b/c"
    try:
      self.assertEqual(self.xmsconf.sweep_repos(self.home), ["a/", "b/c"])
    finally:
      del os.environ["XMS_SWEEP_REPOS"]

  def test_lang(self):
    db = {"collections": {"meta": {"config": {"lang": "zh"}}}}
    self.assertEqual(self.xmsconf.lang(db), "en")  # daemon messages are English-only
    self.assertEqual(self.xmsconf.lang({"collections": {}}), "en")
    self.assertIn("push", self.xmsconf.text("en", "fwd_policy"))

  def test_owner_name_and_mentions(self):
    import inbox_collect as ic  # pylint: disable=g-import-not-at-top
    me = lambda who: (who or "").startswith("owner@")
    ctx = {"cfg": {}, "state": {}, "new_state": {}, "from_me": {"assembled_context": [{"content": [
        {"text": "[Read Message]\nSender: Someone Else <else@example.com>\nTime: t\nText: hi\n"
                 "[Read Message]\nSender: Sam Owner <owner@example.com>\nTime: t\nText: yo"}]}]}}
    os.environ.pop("XMS_OWNER_NAME", None)
    self.assertEqual(ic.owner_name(me, ctx), "Sam Owner")
    self.assertEqual(ctx["new_state"]["owner_name"], "Sam Owner")
    self.assertTrue(ic.mentions_owner("@Sam Owner can you look", "Sam Owner", "owner"))
    self.assertFalse(ic.mentions_owner("@Samantha hi", "Sam Owner", "owner"))
    self.assertTrue(ic.mentions_owner("cc owner@example.com", "", "owner"))
    os.environ["XMS_OWNER_NAME"] = "Explicit Name"
    try:
      self.assertEqual(ic.owner_name(me, {"cfg": {}, "state": {}, "new_state": {}}), "Explicit Name")
    finally:
      del os.environ["XMS_OWNER_NAME"]

  def test_default_topics_empty(self):
    import inbox_collect as ic  # pylint: disable=g-import-not-at-top
    os.environ["XMS_HOME"], old = self.home, os.environ["XMS_HOME"]
    try:
      self.assertEqual(ic.topics(), [])
      self.assertTrue(os.path.exists(os.path.join(self.home, "topics.txt")))
    finally:
      os.environ["XMS_HOME"] = old


class CliTest(unittest.TestCase):

  def test_hubctl_list_rejects_doc_path(self):
    import hubctl  # pylint: disable=g-import-not-at-top
    db = os.path.join(tempfile.mkdtemp(prefix="xms-cli-test-"), "hub.json")
    self.assertEqual(hubctl.main(["--db", db, "add", "items/a", '{"title":"A"}']), 0)
    self.assertEqual(hubctl.main(["--db", db, "list", "items/a"]), 1)

  def test_tell_never_raises(self):
    import xms_tell  # pylint: disable=g-import-not-at-top
    os.environ["XMS_AMPLIO_CLI"] = "/nonexistent/amplio"
    try:
      self.assertIn("not configured", xms_tell.tell("run1", "hi", session="s"))
    finally:
      del os.environ["XMS_AMPLIO_CLI"]
    orig = xms_tell.subprocess.run
    def boom(*a, **k):
      raise xms_tell.subprocess.TimeoutExpired("amplio", 30)
    xms_tell.subprocess.run = boom
    os.environ["XMS_AMPLIO_CLI"] = "sh"
    try:
      self.assertIn("timed out", xms_tell.tell("run1", "hi", session="s"))
    finally:
      xms_tell.subprocess.run = orig
      del os.environ["XMS_AMPLIO_CLI"]


class ToolsTest(unittest.TestCase):
  """External CLIs are optional: missing ones are skipped, never fatal."""

  def setUp(self):
    import xmsconf  # pylint: disable=g-import-not-at-top
    self.xmsconf = xmsconf
    self.home = tempfile.mkdtemp(prefix="xms-tools-test-")
    self.old_home, os.environ["XMS_HOME"] = os.environ["XMS_HOME"], self.home

  def tearDown(self):
    os.environ["XMS_HOME"] = self.old_home
    for k in ("XMS_CL_CLI", "XMS_CHAT_CLI", "XMS_MAIL_CLI"):
      os.environ.pop(k, None)

  def test_resolution(self):
    x = self.xmsconf
    self.assertIsNone(x.tool("cl"))  # optional tools default to "not configured"
    os.environ["XMS_CL_CLI"] = "sh"
    self.assertTrue(x.tool("cl").endswith("/sh"))
    os.environ["XMS_CL_CLI"] = "/nonexistent/cl-cli"
    self.assertIsNone(x.tool("cl"))
    del os.environ["XMS_CL_CLI"]
    with open(os.path.join(self.home, "tools.json"), "w") as f:
      json.dump({"chat": "sh", "gh_hosts": ["Git.Example.com"], "cl_url": "https://review.example.com/c/{num}"}, f)
    self.assertTrue(x.tool("chat").endswith("/sh"))
    self.assertIn("git.example.com", x.gh_hosts())
    self.assertEqual(ledger.key_url("https://review.example.com/c/42"), "cl:42")
    os.environ["XMS_CHAT_CLI"] = ""  # env wins, and "" means off
    self.assertIsNone(x.tool("chat"))
    with self.assertRaises(x.ToolMissing):
      x.need("chat")

  def test_watcher_skips_missing_tool(self):
    import hubdb  # pylint: disable=g-import-not-at-top
    db = os.path.join(self.home, "hub.json")
    hubdb.ensure_exists(db)
    hubdb.run_op(db, "add", "items/c", {"title": "CL", "status": "waiting_others",
                                         "watch": {"kind": "cl", "ref": "42", "seen": "2026-10-01T00:00:00Z"}})
    orig, _watch.fetch = _watch.fetch, REAL_FETCH
    try:
      errs = set()
      self.assertEqual(_watch.check_round(db, lambda w: False, errs), [])
      self.assertEqual(_watch.check_round(db, lambda w: False, errs), [])  # still quiet, never escalates
    finally:
      _watch.fetch = orig
    self.assertTrue(hubdb.read(db)["collections"]["items"]["c"]["watch"]["error"].startswith("skipped"))

  def test_inbox_sources_need_their_cli(self):
    import inbox_collect as ic  # pylint: disable=g-import-not-at-top
    cfg = dict(ic.DEFAULTS)
    self.assertEqual(ic.available_sources(cfg), [])
    os.environ["XMS_MAIL_CLI"] = "sh"
    self.assertEqual(ic.available_sources(cfg), ["gmail"])


if __name__ == "__main__":
  unittest.main()
