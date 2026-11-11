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

"""Notify another amplio run's live session as "xiaomishu".

Usage: xms_tell.py RUN_ID [--session S] [--sender NAME] -      (message text on stdin)
The session defaults to the run's live top-level session (watch.amplio_session).
"""

import sys
sys.dont_write_bytecode = True

import argparse
import json
import os
import subprocess

sys.path.insert(0, os.path.dirname(os.path.realpath(__file__)))
import xmsconf  # noqa: E402
from watch import amplio_session  # noqa: E402


def tell(run_id, text, session=None, sender="xiaomishu"):
  """-> None on success, else a short error string."""
  try:
    sid = session or amplio_session(run_id)
    cli = xmsconf.need("amplio")
    if not sid:
      return "no session"
    body = json.dumps({"sender": sender, "content": text}, ensure_ascii=False)
    p = subprocess.run([cli, "client", "api", "-X", "POST", f"/api/runs/{run_id}/sessions/{sid}/notify",
                        "--data", "-"], input=body, capture_output=True, text=True, timeout=30)
  except (RuntimeError, subprocess.SubprocessError, OSError) as e:
    return str(e)[:160] or type(e).__name__
  return None if p.returncode == 0 else (p.stderr or p.stdout).strip()[:160]


def main(argv=None):
  ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
  ap.add_argument("run_id")
  ap.add_argument("text", help="'-' = read stdin")
  ap.add_argument("--session")
  ap.add_argument("--sender", default="xiaomishu")
  a = ap.parse_args(argv)
  text = sys.stdin.read() if a.text == "-" else a.text
  if not text.strip():
    sys.exit("empty message")
  err = tell(a.run_id, text, a.session, a.sender)
  if err:
    sys.exit(f"notify failed: {err}")
  print("sent")


if __name__ == "__main__":
  main()
