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

"""xiaomishu-work hub server (stdlib only).

GET  /          -> ../assets/hub.html (relative to this script)
GET  /api/db    -> the whole hub.json (ETag from file mtime/size; 304 on If-None-Match)
POST /api/op    -> {"op": "add"|"update"|"set"|"delete", "path": "items[/id]",
                    "data": {...}, "if_version": int?}   (semantics: hubdb.apply_op)

Every request needs the token (cookie, X-XMS-Token header, or ?token= once, which
sets the cookie). Token file: $XMS_TOKEN_FILE > $XMS_HOME/.token (created if missing).

Usage: serve.py [--port 8777] [--host 0.0.0.0] [--db PATH]
       env fallbacks: $XMS_PORT, $XMS_HOST, $XMS_DB > $XMS_HOME/hub.json
       ($XMS_HOME defaults to ~/xiaomishu); $XMS_QUIET=1 silences access logs.
"""

import argparse
import hmac
import http.server
import json
import os
import secrets
import stat
import sys

sys.dont_write_bytecode = True  # the skill dir may be read-only
import hubdb  # pylint: disable=g-import-not-at-top,wrong-import-position

MAX_BODY = 2 << 20
COOKIE = "xms_token"
COOKIE_MAX_AGE = 90 * 24 * 3600

UNAUTH_HTML = """<!doctype html><meta charset=utf-8><title>xiaomishu</title>
<body style="font-family:sans-serif;max-width:36em;margin:4em auto;line-height:1.6">
<h3>Token required</h3>
<p>On the server machine run:</p>
<pre style="background:#eef;padding:.6em">cat {path}</pre>
<p>then open <code>http://&lt;host&gt;:{port}/?token=&lt;token&gt;</code> (your browser remembers it afterwards).</p>
"""


def amplio_base():
  """Public URL of the local amplio server (from its server.json), or ''."""
  path = os.path.join(os.path.expanduser(os.environ.get("AMPLIO_DATA_DIR") or "~/.amplio"), "server.json")
  try:
    with open(path) as f:
      return (json.load(f).get("url") or "").rstrip("/")
  except (OSError, ValueError):
    return ""


def load_token(path):
  """Reads the token file, creating a random one (mode 600) if missing."""
  try:
    with open(path) as f:
      tok = f.read().strip()
    if tok:
      return tok
  except FileNotFoundError:
    pass
  tok = secrets.token_hex(16)
  os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
  fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
  with os.fdopen(fd, "w") as f:
    f.write(tok + "\n")
  return tok


class Handler(http.server.BaseHTTPRequestHandler):
  server_version = "xiaomishu-work/1"
  db = None  # set in main()
  html = os.path.join(os.path.dirname(hubdb.SCRIPT_DIR), "assets", "hub.html")
  token = None  # set in main()
  token_path = ""
  port = 0

  def log_message(self, fmt, *args):
    if os.environ.get("XMS_QUIET") != "1":
      sys.stderr.write("%s %s\n" % (self.log_date_time_string(), fmt % args))

  def _send(self, status, body, ctype="application/json; charset=utf-8", headers=None):
    if isinstance(body, (dict, list)):
      body = json.dumps(body, ensure_ascii=False)
    if isinstance(body, str):
      body = body.encode("utf-8")
    self.send_response(status)
    self.send_header("Content-Type", ctype)
    self.send_header("Content-Length", str(len(body)))
    self.send_header("Cache-Control", "no-store")
    for k, v in (headers or {}).items():
      self.send_header(k, v)
    self.end_headers()
    if self.command != "HEAD":
      self.wfile.write(body)

  def _query_token(self):
    q = self.path.split("?", 1)[1] if "?" in self.path else ""
    for part in q.split("&"):
      k, _, v = part.partition("=")
      if k == "token":
        return v
    return ""

  def _authed(self):
    """Returns (ok, set_cookie). Accepts cookie, X-XMS-Token header, or ?token=."""
    want = self.token.encode()
    def eq(v):
      return bool(v) and hmac.compare_digest(v.encode(), want)
    cookies = self.headers.get("Cookie", "")
    for c in cookies.split(";"):
      k, _, v = c.strip().partition("=")
      if k == COOKIE and eq(v):
        return True, False
    if eq(self.headers.get("X-XMS-Token", "")):
      return True, False
    if eq(self._query_token()):
      return True, True
    return False, False

  def _cookie_header(self):
    return {"Set-Cookie": f"{COOKIE}={self.token}; Max-Age={COOKIE_MAX_AGE}; Path=/; HttpOnly; SameSite=Lax"}

  def do_HEAD(self):
    self.do_GET()

  def do_GET(self):
    route = self.path.split("?", 1)[0]
    ok, set_cookie = self._authed()
    if not ok:
      if route.startswith("/api/"):
        return self._send(401, {"ok": False, "error": "token required"})
      return self._send(401, UNAUTH_HTML.format(path=self.token_path, port=self.port),
                        "text/html; charset=utf-8")
    if route in ("/", "/index.html", "/hub.html"):
      try:
        with open(self.html, "rb") as f:
          return self._send(200, f.read(), "text/html; charset=utf-8",
                            headers=self._cookie_header() if set_cookie else None)
      except FileNotFoundError:
        return self._send(500, {"ok": False, "error": "hub.html missing"})
    if route == "/api/db":
      try:
        st = os.stat(self.db)
        etag = f'"{st.st_mtime_ns:x}-{st.st_size:x}"'
      except FileNotFoundError:
        etag = '"0"'
      if self.headers.get("If-None-Match") == etag:
        self.send_response(304)
        self.send_header("ETag", etag)
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        return None
      try:
        db = hubdb.read(self.db)
      except ValueError as e:  # includes JSONDecodeError
        return self._send(500, {"ok": False, "error": f"hub.json unreadable: {e}"})
      base = amplio_base()
      if base:  # live run-link base, so a changed amplio port never breaks links
        db.setdefault("collections", {}).setdefault("meta", {}).setdefault("config", {})["runBase"] = base
      return self._send(200, db, headers={"ETag": etag})
    return self._send(404, {"ok": False, "error": "not found"})

  def do_POST(self):
    if self.path.split("?", 1)[0] != "/api/op":
      return self._send(404, {"ok": False, "error": "not found"})
    ok, _ = self._authed()
    if not ok:
      return self._send(401, {"ok": False, "error": "token required"})
    # CSRF belt-and-braces on top of the SameSite=Lax cookie: a browser Origin must match Host.
    origin = self.headers.get("Origin")
    if origin and origin.split("://", 1)[-1] != self.headers.get("Host", ""):
      return self._send(403, {"ok": False, "error": "cross-origin write refused"})
    n = int(self.headers.get("Content-Length") or 0)
    if n <= 0 or n > MAX_BODY:
      return self._send(400, {"ok": False, "error": "missing or oversized body"})
    try:
      req = json.loads(self.rfile.read(n))
      if not isinstance(req, dict):
        raise ValueError("body must be an object")
      res = hubdb.run_op(self.db, req.get("op"), req.get("path"), req.get("data"),
                         req.get("if_version"))
    except hubdb.OpError as e:
      return self._send(e.status, e.payload())
    except (ValueError, TypeError) as e:
      return self._send(400, {"ok": False, "error": str(e)})
    return self._send(200, res)


def main(argv=None):
  ap = argparse.ArgumentParser(description=__doc__,
                               formatter_class=argparse.RawDescriptionHelpFormatter)
  ap.add_argument("--port", type=int, default=int(os.environ.get("XMS_PORT", 8777)))
  ap.add_argument("--host", default=os.environ.get("XMS_HOST", "0.0.0.0"))
  ap.add_argument("--db", default=None, help="DB file (default $XMS_DB or $XMS_HOME/hub.json)")
  args = ap.parse_args(argv)
  Handler.db = hubdb.db_path(args.db)
  if hubdb.ensure_exists(Handler.db):
    print(f"created empty {Handler.db}", file=sys.stderr)
  os.chmod(Handler.db, stat.S_IRUSR | stat.S_IWUSR)
  Handler.token_path = hubdb.token_path()
  Handler.token = load_token(Handler.token_path)
  Handler.port = args.port
  http.server.ThreadingHTTPServer.allow_reuse_address = True
  srv = http.server.ThreadingHTTPServer((args.host, args.port), Handler)
  print(f"xiaomishu-work: {Handler.db} on http://{args.host}:{args.port}/", file=sys.stderr)
  try:
    srv.serve_forever()
  except KeyboardInterrupt:
    pass


if __name__ == "__main__":
  main()
