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

"""Shared hub.json store for serve.py and hubctl.py.

DB shape: {"collections": {col: {id: doc}}, "versions": {"col/id": int}}.
Every mutation goes through `transact`, which holds an exclusive fcntl lock on
`<db>.lock`, re-reads the file, applies the op and writes atomically
(tmp + fsync + rename), so the page and the agent's CLI never clobber each other.
"""

import contextlib
import fcntl
import json
import os
import secrets
import tempfile

COLLECTIONS = ("projects", "items", "meta")
OPS = ("add", "update", "set", "delete")
# The scripts may live somewhere read-only; all state goes under the data home.
SCRIPT_DIR = os.path.dirname(os.path.realpath(__file__))


class OpError(Exception):
  """An op failed; `status` is the matching HTTP status code."""

  def __init__(self, status, message, **extra):
    super().__init__(message)
    self.status = status
    self.extra = extra

  def payload(self):
    return {"ok": False, "error": str(self), **self.extra}


def xms_home():
  """Data home: $XMS_HOME, default ~/xiaomishu."""
  return os.path.abspath(os.path.expanduser(os.environ.get("XMS_HOME") or "~/xiaomishu"))


def db_path(cli_value=None):
  """--db flag > $XMS_DB > $XMS_HOME/hub.json."""
  return os.path.abspath(os.path.expanduser(
      cli_value or os.environ.get("XMS_DB") or os.path.join(xms_home(), "hub.json")))


def token_path():
  """$XMS_TOKEN_FILE > $XMS_HOME/.token."""
  return os.path.abspath(os.path.expanduser(
      os.environ.get("XMS_TOKEN_FILE") or os.path.join(xms_home(), ".token")))


def empty_db():
  return {"collections": {c: {} for c in COLLECTIONS}, "versions": {}}


def normalize(db):
  if not isinstance(db, dict):
    raise ValueError("hub.json root must be an object")
  cols = db.setdefault("collections", {})
  for c in COLLECTIONS:
    cols.setdefault(c, {})
  db.setdefault("versions", {})
  return db


def read(path):
  """Lock-free read; safe because writers always rename a complete file."""
  try:
    with open(path, encoding="utf-8") as f:
      return normalize(json.load(f))
  except FileNotFoundError:
    return empty_db()


def _write_atomic(path, db):
  fd, tmp = tempfile.mkstemp(prefix=".hub-", suffix=".tmp", dir=os.path.dirname(path))
  try:
    with os.fdopen(fd, "w", encoding="utf-8") as f:
      json.dump(db, f, ensure_ascii=False, indent=1, sort_keys=True)
      f.write("\n")
      f.flush()
      os.fsync(f.fileno())
    if os.path.exists(path):
      os.chmod(tmp, os.stat(path).st_mode & 0o777)
    os.replace(tmp, path)
  except BaseException:
    with contextlib.suppress(FileNotFoundError):
      os.unlink(tmp)
    raise


@contextlib.contextmanager
def locked(path):
  with open(path + ".lock", "a") as lf:
    fcntl.flock(lf, fcntl.LOCK_EX)
    try:
      yield
    finally:
      fcntl.flock(lf, fcntl.LOCK_UN)


def transact(path, fn):
  """Runs fn(db) under the lock; writes back only if fn returns normally."""
  with locked(path):
    db = read(path)
    result = fn(db)
    _write_atomic(path, db)
    return result


def ensure_exists(path):
  """Creates an empty DB (and its directory) if missing. Never touches an existing file."""
  os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
  with locked(path):
    if not os.path.exists(path):
      _write_atomic(path, empty_db())
      return True
  return False


def split_path(path):
  parts = [p for p in str(path or "").strip("/").split("/") if p]
  if not parts or len(parts) > 2:
    raise OpError(400, f"bad path {path!r}; want '<collection>' or '<collection>/<id>'")
  if parts[0] not in COLLECTIONS:
    raise OpError(400, f"unknown collection {parts[0]!r}; want one of {COLLECTIONS}")
  return parts[0], (parts[1] if len(parts) == 2 else None)


def new_id(existing):
  while True:
    i = secrets.token_hex(4)
    if i not in existing:
      return i


def get(db, path):
  col, doc_id = split_path(path)
  docs = db["collections"][col]
  if doc_id is None:
    return docs
  if doc_id not in docs:
    raise OpError(404, f"{col}/{doc_id} not found")
  return docs[doc_id]


def apply_op(db, op, path, data=None, if_version=None):
  """Mutates db in place. Returns {"ok", "path", "id", "version", "doc"}.

  add:    path=<col> (random id) or <col>/<id> (must not exist); version=1.
  update: shallow merge into an existing doc; null values are stored as null.
  set:    replace (or create) the doc.
  delete: remove the doc (missing is a 404).
  if_version, when given, must equal the doc's current version (0 = absent),
  otherwise 409.
  """
  if op not in OPS:
    raise OpError(400, f"unknown op {op!r}; want one of {OPS}")
  col, doc_id = split_path(path)
  docs = db["collections"][col]
  if op != "delete" and not isinstance(data, dict):
    raise OpError(400, f"{op} needs a JSON object as data")
  if op == "add":
    if doc_id is None:
      doc_id = new_id(docs)
    elif doc_id in docs:
      raise OpError(409, f"{col}/{doc_id} already exists",
                    current_version=db["versions"].get(f"{col}/{doc_id}", 0))
  elif doc_id is None:
    raise OpError(400, f"{op} needs '<collection>/<id>'")

  key = f"{col}/{doc_id}"
  cur = db["versions"].get(key, 0)
  if if_version is not None and int(if_version) != cur:
    raise OpError(409, f"version mismatch on {key}: have {cur}, got {if_version}",
                  current_version=cur, doc=docs.get(doc_id))
  if op in ("update", "delete") and doc_id not in docs:
    raise OpError(404, f"{key} not found")

  if op == "delete":
    del docs[doc_id]
    db["versions"].pop(key, None)
    return {"ok": True, "path": key, "id": doc_id, "version": 0, "doc": None}
  docs[doc_id] = {**docs[doc_id], **data} if op == "update" else dict(data)
  db["versions"][key] = cur + 1
  return {"ok": True, "path": key, "id": doc_id, "version": cur + 1, "doc": docs[doc_id]}


def run_op(path, op, target, data=None, if_version=None):
  """Locked read-modify-write of a single op against the DB file at `path`."""
  return transact(path, lambda db: apply_op(db, op, target, data, if_version))
