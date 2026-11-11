#!/usr/bin/env bash
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

# xiaomishu-work daemons: page server (start|stop|restart|url), watcher (watch|unwatch),
# inbox collector (inbox|uninbox); status shows all three.
# State (pidfile, log, DB, token) lives in $XMS_HOME (default ~/xiaomishu), never next to
# this script. Processes are tracked by pidfile only: pattern-based pkill/pgrep could hit
# the caller's own shell.
set -u
SELF="$(readlink -f "${BASH_SOURCE[0]}")"
SERVE="$(dirname "$SELF")/serve.py"
export XMS_HOME="${XMS_HOME:-$HOME/xiaomishu}"
PIDF="$XMS_HOME/.serve.pid"; LOG="$XMS_HOME/.serve.log"; PORT="${XMS_PORT:-8777}"

pid() { [[ -f $PIDF ]] && cat "$PIDF"; }
# Alive AND still our serve.py (guards against a recycled pid).
running() {
  local p; p="$(pid)" || return 1
  [[ -n $p ]] && kill -0 "$p" 2>/dev/null && ps -o args= -p "$p" 2>/dev/null | grep -q 'serve\.py'
}

# Wait (up to 90s) until the daemon recorded in $XMS_HOME/<pidfile> has exited.
wait_gone() {
  local pf="$XMS_HOME/$1" who="$2" p; p=$(cat "$pf" 2>/dev/null || true)
  for _ in $(seq 1 90); do { [[ -z "$p" ]] || ! kill -0 "$p" 2>/dev/null; } && { echo "$who stopped"; return 0; }; sleep 1; done
  echo "$who (pid $p) still running after 90s"; return 1
}

# Wait (up to 20s) until daemon $2 has written its pid to $XMS_HOME/$1; fail early if it exits.
wait_started() {
  for _ in $(seq 1 100); do
    [[ "$(cat "$XMS_HOME/$1" 2>/dev/null)" == "$2" ]] && return 0
    kill -0 "$2" 2>/dev/null || return 1
    sleep 0.2
  done
  return 1
}

case "${1:-}" in
  start)
    running && { echo "already running (pid $(pid))"; exit 0; }
    mkdir -p "$XMS_HOME" && chmod 700 "$XMS_HOME"
    XMS_QUIET=1 PYTHONDONTWRITEBYTECODE=1 nohup python3 "$SERVE" --port "$PORT" >>"$LOG" 2>&1 </dev/null &
    echo $! >"$PIDF"
    sleep 1
    if running; then echo "started pid $(pid) on :$PORT (data: $XMS_HOME)"
    else echo "failed; see $LOG"; rm -f "$PIDF"; exit 1; fi ;;
  stop)
    if running; then
      p="$(pid)"; kill "$p"
      for _ in $(seq 20); do kill -0 "$p" 2>/dev/null || break; sleep 0.1; done
      echo stopped
    else echo "not running"; fi
    rm -f "$PIDF" ;;
  restart) "$SELF" stop; "$SELF" start ;;
  status)
    running && echo "server:    running pid $(pid) on :$PORT (data: $XMS_HOME)" || echo "server:    not running"
    for d in watch:watch.py inbox:inbox_collect.py; do
      f="$XMS_HOME/.${d%%:*}.pid"; p="$([[ -f $f ]] && cat "$f")"
      if [[ -n $p ]] && grep -qs "${d#*:}" "/proc/$p/cmdline"; then printf '%-10s running pid %s\n' "${d%%:*}:" "$p"
      else printf '%-10s not running\n' "${d%%:*}:"; fi
    done ;;
  url)
    tf="${XMS_TOKEN_FILE:-$XMS_HOME/.token}"
    [[ -r $tf ]] || { echo "no token yet ($tf); run: $0 start"; exit 1; }
    echo "http://$(hostname -f 2>/dev/null || hostname):$PORT/?token=$(tr -d '[:space:]' <"$tf")" ;;
  watch)
    # Background watcher; notifications go to $XMS_NOTIFY (e.g. "amplio-notify -").
    shift; WATCH="$(dirname "$SELF")/watch.py"; mkdir -p "$XMS_HOME" && chmod 700 "$XMS_HOME"
    nohup python3 "$WATCH" "$@" >>"$XMS_HOME/.watch.log" 2>&1 & NEW=$!
    wait_started .watch.pid "$NEW" && echo "watcher pid $NEW (log: $XMS_HOME/.watch.log)" || { echo "watcher did NOT start; $XMS_HOME/.watch.log says (if another one is running: $0 unwatch):"; tail -2 "$XMS_HOME/.watch.log"; exit 1; } ;;
  unwatch) touch "$XMS_HOME/.watch.stop"; wait_gone .watch.pid watcher ;;
  inbox)
    # Background inbox collector; batches go to the triage run $XMS_TRIAGE_RUN.
    shift; INBOX="$(dirname "$SELF")/inbox_collect.py"; mkdir -p "$XMS_HOME" && chmod 700 "$XMS_HOME"
    nohup python3 "$INBOX" "$@" >>"$XMS_HOME/.inbox.log" 2>&1 & NEW=$!
    wait_started .inbox.pid "$NEW" && echo "collector pid $NEW (log: $XMS_HOME/.inbox.log)" || { echo "collector did NOT start; $XMS_HOME/.inbox.log says (if another one is running: $0 uninbox):"; tail -2 "$XMS_HOME/.inbox.log"; exit 1; } ;;
  uninbox) touch "$XMS_HOME/.inbox.stop"; wait_gone .inbox.pid collector ;;
  *) echo "usage: $0 start|stop|restart|status|url|watch [watch.py args]|unwatch|inbox [inbox_collect.py args]|uninbox"; exit 2 ;;
esac
