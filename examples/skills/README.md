# Skill examples

Skills are directories with a `SKILL.md` that amplio agents find through recall
(see [what the agent knows](../../docs/guide/prompt-context.md) and
[configuration](../../docs/guide/configuration.md#recall-skills-and-lessons)).
The examples here show patterns, not a catalog: copy what is useful and adapt it.

amplio itself ships no skills, and nothing here is loaded unless you add it:
point `[skills] dirs` at this directory, or copy the skills you want into your
own. These are examples, not a maintained library: they may change or go away,
and they aren't tuned for your setup. Keep your own skills in your own directory
or repository; amplio reads any number of skill directories, layered in order.

## xiaomishu-work: one skill coordinating many runs

[`xiaomishu-work/`](xiaomishu-work/) shows how a single skill can tie several amplio
runs together. When you have a dozen runs going, each one stalls at some point
waiting on you or on someone else, and it is easy to forget which. The skill gives
all runs one shared to-do store and a few rules for using it:

```text
                 you (web page)
                      |
   secretary run ---- hub.json ---- project run A, project run B, ...
   (coordinator)    (hubctl, locked      (each updates only its own project)
                     atomic writes)
                      |
                   watcher ---- polls watched PRs ----> forwards new activity
                                                        to the owning run
```

- **A coordinating agent (the "secretary")** onboards runs: it drafts a project per
  run, shows you, writes it after you approve, and tells the run about it with
  `amplio client api .../notify`.
- **Each project run** reads its project at the start and updates it when the ball
  moves: an item done, a question for you (`waiting_me`), a reviewer to wait on
  (`waiting_others`), and a one-line `resume` for where to pick up.
- **The shared store** is one JSON file behind a small CLI (`hubctl`). Every write
  takes a file lock and replaces the file atomically; `--if-version` turns lost
  updates into a 409 the agent must resolve. A stdlib-only web page shows it all.
- **A watcher** polls the PRs and issues items point at. New review activity is
  flagged on the page, sent to you in one batched notice, and forwarded to the run
  that owns the work, with a policy on when it may push without asking you.
- **A ledger** records what was already reported, so the same comment seen through
  the API, a notification email, and a triage run reaches you once.

Optional pieces (an inbox collector and triage run for chat and mail, and watches on
code-review CLs, chat spaces, and group membership) depend on CLIs that are not part
of this example; they are skipped when those tools aren't configured.

### Try it

You need Python 3.9+, and for the watcher, [`gh`](https://cli.github.com/) logged in.

```bash
cd examples/skills/xiaomishu-work
export XMS_HOME=/tmp/xms-demo            # all state goes here
python3 scripts/test_ledger.py           # offline tests
XMS_PORT=8777 scripts/xms.sh start
scripts/xms.sh url                       # open it; add #demo-en to see sample data
python3 scripts/hubctl.py add projects/demo-run '{"title":"Try the hub","run_id":"demo-run","stage":"doing"}'
python3 scripts/hubctl.py add items '{"title":"Read the README","project":"demo-run","status":"todo"}'
python3 scripts/hubctl.py add items '{"title":"Follow a PR","project":"demo-run","status":"waiting_others","watch":{"kind":"gh_pr","ref":"example-org/example-repo#123"}}'
XMS_FORWARD=0 python3 scripts/watch.py --once --dry-run   # what the watcher would do
scripts/xms.sh stop
```

To use it with amplio, add `examples/skills` to `[skills] dirs`, start a
coordinator run, and ask it to onboard your other runs into the hub. Setup, the
daemons, and every setting: [xiaomishu-work/README.md](xiaomishu-work/README.md).
