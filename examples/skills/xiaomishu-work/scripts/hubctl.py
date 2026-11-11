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

"""CLI over hub.json with the same op semantics as serve.py's POST /api/op.

  hubctl.py get items/abc            print one doc (or a whole collection: get items)
  hubctl.py list items [--where status=waiting_me] [--brief]
  hubctl.py add items '{"title":"..."}'          (or add items/<id> '{...}')
  hubctl.py update items/abc '{"status":"done"}' [--if-version 3]
  hubctl.py set projects/x '{...}' [--if-version N]
  hubctl.py delete items/abc [--if-version N]
  hubctl.py version items/abc

DB: --db PATH > $XMS_DB > $XMS_HOME/hub.json ($XMS_HOME defaults to ~/xiaomishu).
Data may be '-' to read stdin.
Exit codes: 0 ok, 1 op error (JSON on stderr; 409 = version conflict), 2 usage.
"""

import argparse
import json
import sys

sys.dont_write_bytecode = True  # the skill dir may be read-only
import hubdb  # pylint: disable=g-import-not-at-top,wrong-import-position


def _data(s):
  raw = sys.stdin.read() if s == "-" else s
  try:
    return json.loads(raw)
  except json.JSONDecodeError as e:
    raise hubdb.OpError(400, f"data is not valid JSON: {e}") from e


def _where(pairs):
  conds = []
  for p in pairs or []:
    k, _, v = p.partition("=")
    conds.append((k, None if v == "null" else v))
  return lambda d: all(
      (d.get(k) is None) if v is None else str(d.get(k)) == v for k, v in conds)


def _print(x):
  print(json.dumps(x, ensure_ascii=False, indent=1))


def main(argv=None):
  ap = argparse.ArgumentParser(description=__doc__,
                               formatter_class=argparse.RawDescriptionHelpFormatter)
  ap.add_argument("--db", default=None, help="DB file (default $XMS_DB or $XMS_HOME/hub.json)")
  # Also accept --db after the subcommand; SUPPRESS keeps it from clobbering the global one.
  common = argparse.ArgumentParser(add_help=False)
  common.add_argument("--db", default=argparse.SUPPRESS, help=argparse.SUPPRESS)
  sub = ap.add_subparsers(dest="cmd", required=True)
  sub.add_parser("get", parents=[common]).add_argument("path")
  sub.add_parser("version", parents=[common]).add_argument("path")
  ls = sub.add_parser("list", parents=[common])
  ls.add_argument("collection")
  ls.add_argument("--where", action="append", metavar="FIELD=VALUE",
                  help="repeatable; 'null' matches null/absent")
  ls.add_argument("--brief", action="store_true", help="one line per doc: id, status/stage, title")
  for op in hubdb.OPS:
    p = sub.add_parser(op, parents=[common])
    p.add_argument("path")
    if op != "delete":
      p.add_argument("data")
    p.add_argument("--if-version", type=int, default=None)
  args = ap.parse_args(argv)
  path = hubdb.db_path(args.db)

  try:
    if args.cmd in hubdb.OPS:
      hubdb.ensure_exists(path)
      data = _data(args.data) if args.cmd != "delete" else None
      _print(hubdb.run_op(path, args.cmd, args.path, data, args.if_version))
    elif args.cmd == "get":
      _print(hubdb.get(hubdb.read(path), args.path))
    elif args.cmd == "version":
      col, doc_id = hubdb.split_path(args.path)
      print(hubdb.read(path)["versions"].get(f"{col}/{doc_id}", 0))
    elif args.cmd == "list":
      if hubdb.split_path(args.collection)[1] is not None:
        raise hubdb.OpError(400, f"list takes a collection name, not {args.collection!r}; use get for one doc")
      docs = hubdb.get(hubdb.read(path), args.collection)
      keep = _where(args.where)
      docs = {k: v for k, v in docs.items() if keep(v)}
      if args.brief:
        for k, v in docs.items():
          print(f"{k}\t{v.get('status') or v.get('stage') or ''}\t{v.get('title', '')}")
      else:
        _print(docs)
  except hubdb.OpError as e:
    print(json.dumps(e.payload(), ensure_ascii=False), file=sys.stderr)
    return 1
  return 0


if __name__ == "__main__":
  sys.exit(main())
