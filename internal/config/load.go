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

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Config is the persistent server/runtime configuration, loaded from
// <data-dir>/config.toml. Go is the single source of truth: DefaultConfig holds
// the fallback values and the TOML file overrides only the keys it sets.
type Config struct {
	Listen        string      `toml:"listen"`          // HTTP bind address for `serve`
	DB            string      `toml:"db"`              // sqlite path; defaults to <data-dir>/amplio.db
	Token         string      `toml:"token"`           // web auth token; random per `serve` when empty
	SystemLLMHQ   string      `toml:"system_llm_hq"`   // observer phase summaries / reports (server-level)
	SystemLLMFast string      `toml:"system_llm_fast"` // observer step summaries (server-level)
	EmbedModel    string      `toml:"embed_model"`     // recall embedder: "<backend>:<model>", "local" or "none"
	Run           RunDefaults `toml:"run"`             // defaults applied to new runs
	// Bridge names LLM bridge endpoints, so a spec can say WHICH link
	// (bridge{endpoint=corp}:model) instead of repeating where it is and which
	// variable holds its token. Read at startup like everything else here:
	// moving a bridge means editing this and restarting, which is the right
	// trade for something that changes rarely.
	Bridge map[string]BridgeEndpoint `toml:"bridge"`
	// LendLLM is the bind address for the LLM lending listener, e.g.
	// "127.0.0.1:26760". Empty (the default) disables lending entirely.
	//
	// A SEPARATE listener, not a route on the main one.
	LendLLM string `toml:"lend_llm"`
	// LendLLMTokenEnv names the variable holding the bearer token the lending
	// listener requires. Its own secret.
	LendLLMTokenEnv string        `toml:"lend_llm_token_env"`
	Skills          SkillsConfig  `toml:"skills"`  // skill corpus sources
	Lessons         LessonsConfig `toml:"lessons"` // lesson corpus policy
	// ResponseRewrite optionally restates a chatbot's conclusion messages in
	// plainer prose, shown alongside the original in the UI. Opt-in per model:
	// an empty For list (or an absent block) disables it entirely.
	ResponseRewrite ResponseRewrite `toml:"response_rewrite"`
	// AmplioBinPaths are directories prepended to $PATH at startup so amplio's
	// shipped 1p CLI tools (e.g. web_search) resolve by bare name for both our
	// probes and the agent's bash subprocesses. Omitted → the built-in default;
	// explicit empty list → none.
	AmplioBinPaths []string `toml:"amplio_bin_paths"`
}

// BridgeEndpoint is one [bridge.<name>] section: how to reach a bridge. What to
// ASK IT FOR stays in the spec — this table is only ever about the link.
// ResponseRewrite configures the conclusion-message rewriter.
//
// Opt-in is per MODEL rather than per run or globally: whether a plainer
// restatement helps depends on who writes the original, and an operator who
// picks a model is the one who knows. For lists the models whose runs get it.
type ResponseRewrite struct {
	// Model is the spec that does the rewriting. Required when For is non-empty:
	// a rewrite the operator did not choose the model for is worse than a config
	// error, so this fails at startup rather than defaulting to a system tier.
	Model string `toml:"model"`
	// For names the RUN models that opt in. An entry matches a run whose spec is
	// equal to it, or whose nickname or short label is — the same three ways a
	// bridge handle names a model, so "opus-5 · xhigh" works and there is no
	// second naming scheme to learn. No wildcards.
	For []string `toml:"for"`
	// Prompt replaces the built-in rewrite prompt wholesale. Plain text, no
	// templating: it becomes the system prompt, and the message being rewritten
	// is the only user turn.
	Prompt string `toml:"prompt"`
}

type BridgeEndpoint struct {
	URL         string `toml:"url"`          // https://host:port, http://…, or unix:///path
	TokenEnv    string `toml:"token_env"`    // variable holding the bearer token
	IdleTimeout string `toml:"idle_timeout"` // default for this link; a spec may override
}

// SkillsConfig is the [skills] section: where to scan for SKILL.md files.
type SkillsConfig struct {
	// Dirs are skill source directories, layered in order (last-wins on
	// same-named skills). Omitted → the built-in default; an explicit empty list
	// → skills disabled. See Config.SkillDirs.
	Dirs    []string `toml:"dirs"`
	Blocked []string `toml:"blocked"` // skill names to exclude from all sources
	// Initial is how many skills, ranked by relevance to the task, are listed
	// at session start (in addition to pinned ones). Omitted (nil) →
	// DefaultSkillsInitial; 0 lists none. A pointer so an absent key is
	// distinguishable from an explicit 0. See Config.SkillsInitial.
	Initial *int `toml:"initial"`
	// Pinned are skill source directories (entries of Dirs) whose skills are
	// ALWAYS listed at session start, regardless of relevance and outside the
	// Initial count. For a small, domain-specific skill set the operator wants
	// every agent to know about.
	Pinned []string `toml:"pinned"`
}

// DefaultSkillsInitial is how many relevance-ranked skills are listed at
// session start when [skills].initial is omitted.
const DefaultSkillsInitial = 5

// SkillsInitial returns how many relevance-ranked skills to list at session
// start: [skills].initial, or DefaultSkillsInitial when omitted. A negative
// value reads as 0.
func (c Config) SkillsInitial() int {
	if c.Skills.Initial == nil {
		return DefaultSkillsInitial
	}
	return max(*c.Skills.Initial, 0)
}

// LessonsConfig is the [lessons] section: what an instance may do with the
// lessons mined from past runs.
type LessonsConfig struct {
	// Search enables the AGENT-FACING lesson corpus: recall_search over
	// lessons, recall_load of a lesson: handle, and the run-start seed.
	// Omitted (nil) → enabled. Setting it false gives a controlled run full
	// isolation from what other runs learned, while end-of-run mining, lesson
	// scoring, and the operator's /recall page keep working — this instance
	// still CONTRIBUTES lessons, it just doesn't READ them.
	//
	// A pointer so an absent key is distinguishable from an explicit false: a
	// plain bool would make every zero-valued Config silently mean "off".
	Search *bool `toml:"search"`
}

// DefaultSkillsDir is the fallback when [skills].dirs is omitted from
// config.toml. Empty by default; a 1P init in load_internal.go overrides it
// to the canonical 1P skill tree. Set [skills].dirs explicitly to point at
// your own directory of SKILL.md files.
var DefaultSkillsDir = ""

// EmbedModelLocal and EmbedModelNone are the embed_model values that name no
// API model: the built-in local embedder (BM25 only in a binary built without
// its table), and recall switched off.
const (
	EmbedModelLocal = "local"
	EmbedModelNone  = "none"
)

// DefaultEmbedModel is the embedding model used for recall when embed_model is
// unset: the built-in local embedder in OSS, so recall works with no API; a 1P
// init in load_internal.go overrides it.
var DefaultEmbedModel = EmbedModelLocal

// DefaultListen is the serve bind address when [listen] is unset. localhost in
// OSS (safe default — not exposed to the network).
var DefaultListen = "localhost:26759"

// DefaultAmplioBinPath is the fallback for amplio_bin_paths in config.toml
// (prepended to $PATH at startup). Empty in OSS; a 1P init in
// load_internal.go overrides it to the corp release dir holding amplio's
// shipped 1P CLI tools.
var DefaultAmplioBinPath = ""

// BinPaths returns the directories to prepend to $PATH at startup, falling back
// to the built-in default only when amplio_bin_paths is omitted entirely (nil).
// An explicit empty list disables the prepend.
func (c Config) BinPaths() []string {
	if c.AmplioBinPaths == nil {
		return []string{DefaultAmplioBinPath}
	}
	return c.AmplioBinPaths
}

// SkillDirs returns the configured skill source directories, falling back to the
// built-in default only when [skills].dirs is omitted entirely (nil). An
// explicit empty list disables skills.
func (c Config) SkillDirs() []string {
	if c.Skills.Dirs == nil {
		return []string{DefaultSkillsDir}
	}
	return c.Skills.Dirs
}

// LessonSearchEnabled reports whether agents may search the lesson corpus.
// Default (key absent) is enabled.
func (c Config) LessonSearchEnabled() bool {
	return c.Lessons.Search == nil || *c.Lessons.Search
}

// EmbedModelOrDefault returns the configured embedding model, or the built-in
// default when unset. Callers treat "" like EmbedModelNone (no recall).
func (c Config) EmbedModelOrDefault() string {
	if c.EmbedModel != "" {
		return c.EmbedModel
	}
	return DefaultEmbedModel
}

// DefaultAgentType and DefaultWorkspace are the GLOBAL fallbacks for a run's
// agent type and working directory.
const (
	DefaultAgentType = "standard_agent"
	DefaultWorkspace = "."
)

// RunDefaults is the [run] section. Currently it carries only the agent model menu.
type RunDefaults struct {
	LLMs []string `toml:"llms"` // agent model menu; the first is the default
}

// DefaultLLM is the default agent model (the first configured), or "" if none.
func (c Config) DefaultLLM() string {
	if len(c.Run.LLMs) > 0 {
		return c.Run.LLMs[0]
	}
	return ""
}

// DefaultConfig is the authoritative fallback. A user's config.toml overrides
// these per-key; anything it omits keeps these values.
func DefaultConfig() Config {
	return Config{
		Listen: DefaultListen,
	}
}

// ConfigPath is the config file location for a data directory.
func ConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "config.toml")
}

// Load reads <dataDir>/config.toml over DefaultConfig. A missing file is fine
// (pure defaults). DB defaults to <dataDir>/amplio.db when unset.
func Load(dataDir string) (Config, error) {
	cfg := DefaultConfig()
	path := ConfigPath(dataDir)
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Defaults only — legitimate when everything comes from flags or the
		// environment, but say so: a missing file and a missing KEY otherwise
		// produce the same "missing required config" error, and the usual cause
		// is a data dir that isn't the one being edited (an unexpanded ~ in
		// --data-dir makes a literal "~" directory).
		slog.Warn("no config file; using defaults", "path", path)
	case err != nil:
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	default:
		if err := toml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	cfg.DB = expandTilde(cfg.DB)
	for i, d := range cfg.Skills.Dirs {
		cfg.Skills.Dirs[i] = expandTilde(d)
	}
	for i, d := range cfg.Skills.Pinned {
		cfg.Skills.Pinned[i] = expandTilde(d)
	}
	if cfg.DB == "" {
		cfg.DB = filepath.Join(dataDir, "amplio.db")
	}
	return cfg, nil
}

// Overrides carries the command-line flag values for the layered knobs, so
// Resolve can apply the FLAG > ENV > CONFIG > DEFAULT precedence uniformly.
// Each field is the raw flag value (""/nil = flag not given). SkillDirsSet
// distinguishes "--skill-dir not passed" from "--skill-dir explicitly cleared",
// since an empty skill-dir list is meaningful (disables skills).
type Overrides struct {
	SystemLLMHQ   string
	SystemLLMFast string
	EmbedModel    string
	SkillDirs     []string
	SkillDirsSet  bool
	LessonSearch  *bool // nil = --lesson-search not passed
}

// Resolve loads <dataDir>/config.toml and overlays the flag/env layers on top,
// yielding the effective Config. Precedence per knob: flag > env > config-file >
// built-in default. The required system tiers (hq + fast) must resolve to a
// non-empty value or Resolve returns an error naming the flag/env to set.
//
// This is the single resolution point for serve / headless run / headless
// resume, replacing the per-command inline checks.
func Resolve(dataDir string, o Overrides) (Config, error) {
	cfg, err := Load(dataDir)
	if err != nil {
		return Config{}, err
	}

	// Scalars: flag > env > (already-loaded config/default).
	cfg.SystemLLMHQ = firstNonEmpty(o.SystemLLMHQ, os.Getenv(EnvSystemLLMHQ), cfg.SystemLLMHQ)
	cfg.SystemLLMFast = firstNonEmpty(o.SystemLLMFast, os.Getenv(EnvSystemLLMFast), cfg.SystemLLMFast)
	cfg.EmbedModel = firstNonEmpty(o.EmbedModel, os.Getenv(EnvEmbedModel), cfg.EmbedModel)

	// Lesson search (tri-state bool): an explicit flag wins, then the env var,
	// then the config key, then the default (enabled). An unparseable env value
	// is an ERROR rather than a fallback — this switch exists to guarantee an
	// isolated run, and silently re-enabling recall because of a typo would
	// contaminate exactly the experiment it was set for.
	if o.LessonSearch != nil {
		cfg.Lessons.Search = o.LessonSearch
	} else if raw := strings.TrimSpace(os.Getenv(EnvLessonSearch)); raw != "" {
		// Empty reads as unset, like every other env layer here: an exported-but-
		// empty variable is a shell artefact, not an instruction.
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s=%q: want a boolean (1/0, true/false)", EnvLessonSearch, raw)
		}
		cfg.Lessons.Search = &v
	}

	// Skill dirs (list, REPLACE semantics): the highest layer that is SET wins
	// wholesale. flag-set (SkillDirsSet) > env-set (LookupEnv) > config (handled
	// by SkillDirs()/the [skills].dirs nil-vs-empty distinction).
	if o.SkillDirsSet {
		cfg.Skills.Dirs = o.SkillDirs
	} else if env, ok := os.LookupEnv(EnvSkillDirs); ok {
		cfg.Skills.Dirs = filepath.SplitList(env)
	}
	for i, d := range cfg.Skills.Dirs {
		cfg.Skills.Dirs[i] = expandTilde(d) // also covers the flag and env forms
	}

	// Required: the system tiers drive the process-global observer/finalizer and
	// reactive compaction; without them the run-hosting modes can't function.
	if cfg.SystemLLMHQ == "" {
		return Config{}, requiredErr("--system-llm-hq", EnvSystemLLMHQ, "system_llm_hq")
	}
	if cfg.SystemLLMFast == "" {
		return Config{}, requiredErr("--system-llm-fast", EnvSystemLLMFast, "system_llm_fast")
	}
	// Fail fast rather than falling back to a system tier: the rewrite is shown
	// to the operator as the agent's own words restated, so which model produced
	// it is a choice, not a default.
	if len(cfg.ResponseRewrite.For) > 0 && cfg.ResponseRewrite.Model == "" {
		return Config{}, fmt.Errorf(
			"[response_rewrite] lists %d model(s) in `for` but sets no `model`: "+
				"name the spec that should do the rewriting, or remove `for` to disable it",
			len(cfg.ResponseRewrite.For))
	}
	return cfg, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func requiredErr(flagName, envVar, tomlKey string) error {
	return fmt.Errorf(
		"missing required config %s: pass %s=<provider:model>, set $%s, or add %s to %s",
		tomlKey, flagName, envVar, tomlKey, ConfigPath(DataDir()),
	)
}
