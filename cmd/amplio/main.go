// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Amplio is an agentic framework for long-horizon autonomous research runs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"amplio/internal/cli"
	"amplio/internal/config"
	"amplio/internal/db"
	"amplio/internal/event"
	"amplio/internal/llm"
	amlog "amplio/internal/log"
	"amplio/internal/observer"
	"amplio/internal/runspec"
	"amplio/internal/runtime"
	"amplio/internal/version"
	"amplio/internal/workspace/resolver"

	// Register agent types.
	_ "amplio/internal/agent/chatbot"
	_ "amplio/internal/agent/standard"

	"github.com/spf13/cobra"
)

// Layered-knob flag vars (root-persistent). Bound in main, read by the
// run-hosting subcommands via resolveConfig, which applies the
// flag > env > config > default precedence through config.Resolve.
var (
	flagSystemLLMHQ   string
	flagSystemLLMFast string
	flagEmbedModel    string
	flagSkillDirs     []string
	flagLessonSearch  bool
)

// resolveConfig loads + layers the effective Config for a run-hosting command.
// cmd is needed to distinguish an explicitly-passed --skill-dir (even empty)
// from an absent one, which controls REPLACE-vs-default for the skill list.
func resolveConfig(cmd *cobra.Command) (config.Config, error) {
	cfg, err := resolveConfigRaw(cmd)
	bridgeEndpoints = cfg.Bridge
	return cfg, err
}

func resolveConfigRaw(cmd *cobra.Command) (config.Config, error) {
	return config.Resolve(config.DataDir(), config.Overrides{
		SystemLLMHQ:   flagSystemLLMHQ,
		SystemLLMFast: flagSystemLLMFast,
		EmbedModel:    flagEmbedModel,
		SkillDirs:     flagSkillDirs,
		SkillDirsSet:  cmd.Flags().Changed("skill-dir"),
		LessonSearch:  lessonSearchOverride(cmd),
	})
}

// lessonSearchOverride returns the --lesson-search value only when the flag was
// actually passed; nil lets the env var / config key decide. Cobra gives a bool
// flag a value either way, so "not passed" has to come from Changed.
func lessonSearchOverride(cmd *cobra.Command) *bool {
	if !cmd.Flags().Changed("lesson-search") {
		return nil
	}
	v := flagLessonSearch
	return &v
}

// shimName is the single-purpose entry point installed in <data-dir>/bin and
// pointed at by $AMPLIO_NOTIFY. Dispatching on argv[0] (the busybox / git-* idiom)
// rather than shipping a wrapper script is deliberate: a symlink adds NO process,
// so `notify` still sees the CALLER as its parent — and that ppid is what stamps
// the sender, letting a revived agent identify and kill a stale notifier. A
// non-exec wrapper would record the wrapper's pid, which exits immediately and
// whose number the kernel then recycles onto some unrelated process.
//
// It also narrows what a new agent can do: the prompt teaches `amplio-notify`,
// and through this name the full CLI is not reachable. $AMPLIO_NOTIFY (the whole
// binary) stays for agents and scripts already written against it.
const shimName = "amplio-notify"

// dispatchShim runs the notify command directly when invoked through the shim,
// reporting whether it handled the call.
func dispatchShim() (handled bool, err error) {
	if filepath.Base(os.Args[0]) != shimName {
		return false, nil
	}
	// No compatibility shimming here on purpose. The old interface still exists
	// unchanged — $AMPLIO_NOTIFY is the binary, and `amplio notify …` works as it
	// always did — so this name is free to have exactly one calling convention.
	// Stripping an optional leading "notify" would have made a message that IS
	// the word "notify" unsendable, and given one call two spellings.
	cmd := notifyCmd()
	cmd.SetArgs(os.Args[1:])
	// The root command sets this; the shim bypasses the root, so set it here too
	// or cobra prints the error and exitCodeFor prints it again.
	cmd.SilenceErrors = true
	return true, cmd.Execute()
}

func main() {
	if handled, err := dispatchShim(); handled {
		if err != nil {
			os.Exit(exitCodeFor(err))
		}
		return
	}
	var (
		dataDir   string
		logLevel  string
		logFormat string
	)
	root := &cobra.Command{
		Use:     "amplio",
		Short:   "Agentic framework for long-horizon autonomous research runs",
		Version: version.Build().String(),
		// Resolve the data dir once, before any subcommand reads config or the DB,
		// and install the process-wide logger so EVERY package's slog.* call goes
		// through one handler with one level. Subcommands may re-Init later (e.g.
		// serve adds a file destination) but the level set here is the floor.
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			if dataDir != "" {
				config.SetDataDir(dataDir)
			}
			// Canonicalize $AMPLIO_DATA_DIR to the RESOLVED value (flag > env >
			// default) so child processes — e.g. a background script calling
			// `amplio notify` — target this instance's data dir, even when the
			// inherited env disagreed with our --data-dir or we took the default.
			_ = os.Setenv(config.EnvDataDir, config.DataDir())

			levelStr := firstNonEmpty(logLevel, os.Getenv("AMPLIO_LOG_LEVEL"))
			lvl, err := amlog.ParseLevel(levelStr)
			if err != nil {
				return err
			}
			amlog.Init(amlog.Options{
				Level:  lvl,
				Format: firstNonEmpty(logFormat, os.Getenv("AMPLIO_LOG_FORMAT"), "text"),
				Writer: os.Stderr,
			})
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true, // we print + map exit codes ourselves below
	}
	// Cobra's default version template prints "appname version X", which puts
	// the word "version" awkwardly between the name and the actual identity
	// (which itself contains channel + commit + time). Replace with a tighter
	// "amplio <identity>" so the line reads naturally end-to-end.
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")

	root.PersistentFlags().StringVar(&dataDir, "data-dir", "",
		"Data directory holding config.toml + DB (default $AMPLIO_DATA_DIR or ~/.amplio)")
	root.PersistentFlags().StringVar(&logLevel, "log-level", "",
		"Log level: debug|info|warn|error (env AMPLIO_LOG_LEVEL; default info)")
	root.PersistentFlags().StringVar(&logFormat, "log-format", "",
		"Log format: text|json (env AMPLIO_LOG_FORMAT; default text)")
	root.PersistentFlags().StringVar(&flagSystemLLMHQ, "system-llm-hq", "",
		"System HQ LLM spec for observer summaries/reports (required; env AMPLIO_SYSTEM_LLM_HQ or config system_llm_hq)")
	root.PersistentFlags().StringVar(&flagSystemLLMFast, "system-llm-fast", "",
		"System fast LLM spec for step summaries/compaction (required; env AMPLIO_SYSTEM_LLM_FAST or config system_llm_fast)")
	root.PersistentFlags().StringVar(&flagEmbedModel, "embed-model", "",
		"Embedding model for recall (env AMPLIO_EMBED_MODEL or config embed_model; empty disables recall)")
	root.PersistentFlags().StringArrayVar(&flagSkillDirs, "skill-dir", nil,
		"Skill source directory (repeatable; env AMPLIO_SKILL_DIRS path-list; or config [skills].dirs)")
	root.PersistentFlags().BoolVar(&flagLessonSearch, "lesson-search", true,
		"Let agents search lessons mined from past runs (env AMPLIO_LESSON_SEARCH or config [lessons].search). "+
			"--lesson-search=false isolates runs from other runs' lessons; mining and scoring still happen")

	root.AddCommand(serveCmd(), notifyCmd(), headlessCmd(), clientCmd())
	if err := root.Execute(); err != nil {
		os.Exit(exitCodeFor(err))
	}
}

// exitCodeFor prints err and returns the process exit code: notify's stable
// codes (usage / unreachable / refused) survive, everything else is 1. Shared
// with the shim path so `amplio notify` and `amplio-notify` agree.
func exitCodeFor(err error) int {
	var ce *codedError
	if errors.As(err, &ce) {
		fmt.Fprintln(os.Stderr, "Error:", ce.msg)
		return ce.code
	}
	fmt.Fprintln(os.Stderr, "Error:", err)
	return 1
}

// runCmd is `headless run`; noReport is the parent's --no-report.
func runCmd(noReport *bool) *cobra.Command {
	var (
		task      string
		workspace string
		llmSpec   string
		agentType string
	)
	cmd := &cobra.Command{
		Use:   "run [task]",
		Short: "Start a run in this process and wait for it to finish (headless)",
		Long: "Run a task to completion in the foreground. This process owns the data" +
			" directory for its lifetime, so it cannot share a data dir with a running" +
			" `serve` — use `amplio client submit` to hand a task to a running server instead.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				task = args[0]
			}
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			return executeRun(cfg, task, workspace, llmSpec, agentType, *noReport)
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "Task description (or pass as a positional arg)")
	cmd.Flags().StringVar(&workspace, "workspace", "", "Working directory (default \".\")")
	cmd.Flags().StringVar(&llmSpec, "llm", "", "Agent LLM spec (default: first entry of config [run] llms)")
	cmd.Flags().StringVar(&agentType, "agent", "", "Agent type: standard_agent | chatbot (default standard_agent)")
	return cmd
}

func executeRun(cfg config.Config, task, workspace, llmSpec, agentType string, noReport bool) error {
	if task == "" {
		return fmt.Errorf("a task is required (positional arg or --task)")
	}
	dataDir := config.DataDir()
	llmSpec = firstNonEmpty(llmSpec, cfg.DefaultLLM())
	if llmSpec == "" {
		return fmt.Errorf("no agent LLM: pass --llm or set run.llms in %s", config.ConfigPath(dataDir))
	}
	workspace = firstNonEmpty(workspace, config.DefaultWorkspace)
	agentType = firstNonEmpty(agentType, config.DefaultAgentType)

	lock, err := lockDataDir(dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Shared, run-independent system. Headless: no broadcaster (no UI) and no
	// live report trigger — the run concludes once and we finalize the report
	// explicitly after waitForRun, below.
	sysEnv, err := setupSystem(ctx, cfg, systemOpts{})
	if err != nil {
		return err
	}
	defer sysEnv.cleanup(ctx)
	mgr, fin := sysEnv.mgr, sysEnv.fin
	stopNotify := startHeadlessNotify(dataDir, sysEnv.store)
	defer stopNotify()

	// Resolve the workspace spec to a concrete path (creation sentinels run
	// their side effects here — the sole fresh-vs-resume difference) and snapshot
	// the operator's AGENTS.md. The OS user is needed locally to resolve a citc
	// workspace.
	wsRoot, agentsMD, err := runspec.Prepare(workspace, os.Getenv("USER"))
	if err != nil {
		return err
	}
	runID, err := mgr.StartRun(ctx, runtime.StartRunConfig{
		RunConfig: config.RunConfig{
			Task:      task,
			Workspace: wsRoot,
			LLM:       llmSpec,
			AgentType: agentType,
			AgentsMD:  agentsMD,
		},
		RootSessionID: config.RootAgentSessionID,
	})
	if err != nil {
		return err
	}
	slog.Info("run started", "run_id", runID, "agent", agentType, "model", llmSpec)
	waitForRun(ctx, mgr, runID)
	// Deterministically produce the run report for this headless run: the observer
	// may exit before processing the conclude. No-op if not autonomous / already
	// reported. Best-effort summaries (the critic falls back to raw events).
	if !noReport {
		fin.OnMainAgentConcluded(ctx, runID)
	}
	return nil
}

// resumeCmd is `headless resume`; noReport is the parent's --no-report.
func resumeCmd(noReport *bool) *cobra.Command {
	var message string
	cmd := &cobra.Command{
		Use:   "resume <run-id>",
		Short: "Resume a previously interrupted run (headless)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			return executeResume(cfg, args[0], message, *noReport)
		},
	}
	cmd.Flags().StringVar(&message, "message", "",
		"Give the root agent this message (as user input) and resume it, even if it had concluded")
	return cmd
}

func executeResume(cfg config.Config, runID, message string, noReport bool) error {
	dataDir := config.DataDir()

	lock, err := lockDataDir(dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Shared, run-independent system. The run's provider/workspace/agent are
	// reconstructed from its stored config, so resume needs no --llm/--workspace
	// flags. System tiers come from current config (the observer + finalizer are
	// process-global, not per-run); the manager resolves the agent provider
	// per-run from run.Config.LLM. Headless: no broadcaster, no live report
	// trigger — we finalize explicitly after waitForRun.
	sysEnv, err := setupSystem(ctx, cfg, systemOpts{})
	if err != nil {
		return err
	}
	defer sysEnv.cleanup(ctx)
	store, mgr, fin := sysEnv.store, sysEnv.mgr, sysEnv.fin
	stopNotify := startHeadlessNotify(dataDir, store)
	defer stopNotify()

	run, err := store.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("run %q not found: %w", runID, err)
	}

	// User input revives even a concluded root, which RecoverRun leaves at rest.
	// The commit notifier respawns the root inside AppendEvent, so it is
	// registered before RecoverRun runs, which then leaves it alone (and does
	// not count it).
	var posted *postedMessage
	if message != "" {
		root, err := store.GetSession(ctx, runID, config.RootAgentSessionID)
		if err != nil {
			return fmt.Errorf("run %q has no %s to give the message to: %w", runID, config.RootAgentSessionID, err)
		}
		id, err := store.AppendEvent(ctx, runID, config.RootAgentSessionID, &event.UserEvent{Content: message})
		if err != nil {
			return fmt.Errorf("post resume message: %w", err)
		}
		posted = &postedMessage{eventID: id, before: *root}
	}
	revived, err := mgr.RecoverRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("recover run: %w", err)
	}
	if revived == 0 && message == "" {
		slog.Info("run is already at rest; nothing to resume", "run_id", runID)
	} else {
		slog.Info("run resumed", "run_id", runID, "sessions", revived, "with_message", message != "", "model", run.Config.LLM)
	}
	waitForRun(ctx, mgr, runID) // returns at once if nothing was revived
	if !noReport {
		fin.OnMainAgentConcluded(ctx, runID) // deterministic report for this headless resume
	}
	if posted != nil && ctx.Err() == nil { // an interrupt is a deliberate stop
		return checkMessageOutcome(ctx, store, runID, *posted)
	}
	return nil
}

// postedMessage is a resume --message: its event, and the root as it was
// before.
type postedMessage struct {
	eventID string
	before  db.SessionRecord
}

// checkMessageOutcome runs once the run is at rest. As for `run`, the exit code
// says whether the command did its job, and the run's outcome is in its state:
// a message that revived nothing (the root couldn't be respawned; the reason is
// in the log) is an error, while a root that ran but didn't reply (it crashed,
// say) is a warning, its status already recorded.
func checkMessageOutcome(ctx context.Context, store db.Store, runID string, m postedMessage) error {
	root, err := store.GetSession(ctx, runID, config.RootAgentSessionID)
	if err != nil {
		return fmt.Errorf("read %s: %w", config.RootAgentSessionID, err)
	}
	events, err := store.GetEvents(ctx, runID, config.RootAgentSessionID, db.EventFilter{})
	if err != nil {
		return fmt.Errorf("read %s's events: %w", config.RootAgentSessionID, err)
	}
	// A revived root changes status (a non-ongoing one goes ongoing first) or
	// records a crash. RecoverRun's marker is not the root's doing.
	revived := root.Status != m.before.Status || root.StatusChangedAt.After(m.before.StatusChangedAt)
	after := false
	for _, e := range events {
		switch ev := e.Event.(type) {
		case *event.AssistantEvent:
			if after {
				return nil // it replied
			}
		case *event.SystemEvent:
			revived = revived || (after && ev.Marker == event.MarkerError)
		}
		after = after || e.EventID == m.eventID
	}
	if !revived {
		return fmt.Errorf("the message did not revive %s; see the log for why", config.RootAgentSessionID)
	}
	slog.Warn("main-agent did not respond to the message", "run_id", runID, "status", root.Status)
	return nil
}

// firstNonEmpty returns the first non-empty string, or "" if all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// bindCLITools prepends the configured amplio bin dirs to $PATH (so shipped 1p
// CLI tools resolve by bare name for our probes and the agent's bash subprocess)
// and warns the operator once about any optional tool that isn't installed.
func bindCLITools(cfg config.Config) {
	cli.BindPaths(cfg.BinPaths())
	cli.PrintStatus(os.Stderr, cli.All())
}

// buildManager wires a run manager and its commit notifier over a store. The
// manager resolves each run's agent provider from its RunConfig.LLM via
// createProvider, so different runs can use different models.
func buildManager(store db.Store, extra ...db.CommitListener) *runtime.RunManager {
	mgr := runtime.NewRunManager(store, createProvider, runtime.NewRunRegistry(), resolver.Wrap)
	// There is one commit listener, and the wake path owns it — so anything else
	// that wants commits is composed in here rather than replacing it. Extras run
	// AFTER the notifier and must not block: this is the write path.
	notifier := runtime.NewCommitNotifier(mgr.RunRegistry(), mgr.RespawnSession, mgr.SessionStatus)
	store.SetCommitListener(func(runID, sessionID string, evt event.Event) {
		notifier(runID, sessionID, evt)
		for _, fn := range extra {
			fn(runID, sessionID, evt)
		}
	})
	return mgr
}

// startObserver installs the run-report finalizer (before starting, so worker
// goroutines never race the field) and launches the process-global observer on
// the shared system-tier providers. The caller Stops it (drains pending
// summaries) before exit. finalizer may be nil (no report generation).
func startObserver(ctx context.Context, store db.Store, fast, hq llm.Provider, finalizer func(context.Context, string)) *observer.Observer {
	obs := observer.New(store, fast, hq, observer.DefaultWorkers)
	obs.SetFinalizer(finalizer)
	obs.Start(ctx)
	return obs
}

// waitForRun blocks until none of the run's agents is running, then logs "run
// at rest": every headless run and resume passes here, so a supervising
// harness can key on that line. Past it the process only finalizes (the run
// report) and drains the observer's summaries, which can take minutes. On
// interrupt it cancels the run instead and logs no rest; until this process
// exits, nothing starts the run's sessions again (a later resume does).
func waitForRun(ctx context.Context, mgr *runtime.RunManager, runID string) {
	reg := mgr.RunRegistry()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	// Sessions register synchronously as they are started or revived, so a
	// run with none registered is already at rest.
	for reg.IsRunActive(runID) {
		select {
		case <-ctx.Done():
			slog.Info("interrupted, cancelling run")
			// CancelAll also closes the registry, in memory. A notification
			// arriving before this process exits (from a background job, or a
			// harness) is stored for a later resume, but must not revive the
			// root here.
			reg.CancelAll()
			time.Sleep(time.Second)
			return
		case <-ticker.C:
		}
	}
	slog.Info("run at rest", "run_id", runID)
}
