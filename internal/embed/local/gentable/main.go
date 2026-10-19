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

// Command gentable builds the built-in embedding table (`make embed-table`):
// it downloads the pinned upstream model into a cache, verifies its checksums,
// derives the table (PCA, int8) and installs it into internal/embed/local/data
// for go:embed. A no-op when the installed table already matches. Any failure
// exits non-zero and says how to build without the table.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"amplio/internal/embed/local"
)

// generatorVersion is bumped whenever the derivation changes the output, so
// cached and installed tables from an older recipe are rebuilt.
const generatorVersion = 1

// source is an upstream model plus how to reduce it.
type source struct {
	Repo               string // e.g. minishlab/potion-base-32M
	Revision           string
	Name               string
	Dims               int
	Tokenizer, Weights pinnedFile
}

// pinned is the model the recipe was evaluated with (MIT-licensed; see
// THIRD_PARTY_NOTICES.txt). Changing it means re-validating recall quality.
var pinned = source{
	Repo:     "minishlab/potion-base-32M",
	Revision: "1e5a03f8eeb2c98b928fbbd846f22f816360919f",
	Name:     "potion-base-32M",
	Dims:     256,
	Tokenizer: pinnedFile{
		Name:   "tokenizer.json",
		SHA256: "7d75cbc54318138807c401b0f0c9721117c628b39de8e8e0edb6cb17e0ee7d18",
		Size:   1_493_150,
	},
	Weights: pinnedFile{
		Name:   "model.safetensors",
		SHA256: "99f6c33204c9231a7391871b7a3c91409b532c8f587a9ea44fc282303d8dec28",
		Size:   129_210_456,
	},
}

func (s source) header(vocab int, tok local.TokenizerConfig) local.Header {
	return local.Header{
		Format: local.TableFormat, Model: s.Repo, Revision: s.Revision, Name: s.Name,
		Dims: s.Dims, Quant: local.QuantInt8Row, Generator: generatorVersion, Vocab: vocab, Tokenizer: tok,
	}
}

// matches reports whether h was derived from s by this generator version.
func (s source) matches(h local.Header) bool {
	want := s.header(h.Vocab, h.Tokenizer)
	return h.Format == want.Format && h.Model == want.Model && h.Revision == want.Revision &&
		h.Name == want.Name && h.Dims == want.Dims && h.Quant == want.Quant && h.Generator == want.Generator
}

// cacheKey names the source's cache directory.
func (s source) cacheKey() string {
	return strings.ReplaceAll(s.Repo, "/", "--") + "@" + s.Revision
}

func (s source) url(base string, f pinnedFile) string {
	return strings.TrimRight(base, "/") + "/" + s.Repo + "/resolve/" + s.Revision + "/" + f.Name
}

// config is one generator run.
type config struct {
	src      source
	baseURL  string // mirror root; files are at <base>/<repo>/resolve/<revision>/<file>
	cacheDir string // per-source cache directory
	outDir   string // install directory (internal/embed/local/data)
	fetch    fetcher
	log      io.Writer
}

// run installs the table into cfg.outDir, deriving it (and downloading its
// inputs) only when neither the installed nor the cached table matches.
func run(ctx context.Context, cfg config) error {
	dst := filepath.Join(cfg.outDir, local.TableFile)
	if h, err := readTableHeader(dst); err == nil && cfg.src.matches(h) {
		fmt.Fprintf(cfg.log, "embed-table: %s is up to date (%s@%.12s, %d dims)\n", dst, h.Name, h.Revision, h.Dims)
		return nil
	}
	cached := filepath.Join(cfg.cacheDir, fmt.Sprintf("table-pca%d-%s-g%d.bin", cfg.src.Dims, local.QuantInt8Row, generatorVersion))
	if h, err := readTableHeader(cached); err == nil && cfg.src.matches(h) {
		fmt.Fprintf(cfg.log, "embed-table: using cached table %s\n", cached)
		return install(cached, dst, cfg.log)
	}

	tokPath, err := cfg.fetch.ensure(ctx, cfg.src.url(cfg.baseURL, cfg.src.Tokenizer), cfg.cacheDir, cfg.src.Tokenizer)
	if err != nil {
		return err
	}
	weightsPath, err := cfg.fetch.ensure(ctx, cfg.src.url(cfg.baseURL, cfg.src.Weights), cfg.cacheDir, cfg.src.Weights)
	if err != nil {
		return err
	}
	tokJSON, err := os.ReadFile(tokPath) //nolint:gosec // our own cache path
	if err != nil {
		return err
	}
	weights, err := os.ReadFile(weightsPath) //nolint:gosec // our own cache path
	if err != nil {
		return err
	}
	// The cache was verified when written; re-check what was read, so a file
	// altered since can never reach the binary.
	if err := verifyBytes(tokJSON, cfg.src.Tokenizer); err != nil {
		return fmt.Errorf("%s: %w", tokPath, err)
	}
	if err := verifyBytes(weights, cfg.src.Weights); err != nil {
		return fmt.Errorf("%s: %w", weightsPath, err)
	}

	start := time.Now()
	fmt.Fprintf(cfg.log, "embed-table: deriving the table (PCA to %d dims, int8)\n", cfg.src.Dims)
	t, err := derive(cfg.src, tokJSON, weights)
	if err != nil {
		return fmt.Errorf("derive table: %w", err)
	}
	out, err := t.Marshal()
	if err != nil {
		return fmt.Errorf("derive table: %w", err)
	}
	if err := writeAtomic(cached, out); err != nil {
		return fmt.Errorf("cache table: %w", err)
	}
	fmt.Fprintf(cfg.log, "embed-table: derived %s (%.1f MB) in %s\n", cached, float64(len(out))/1e6, time.Since(start).Round(time.Millisecond))
	return install(cached, dst, cfg.log)
}

// readTableHeader validates the whole table file at path and returns its header.
func readTableHeader(path string) (local.Header, error) {
	b, err := os.ReadFile(path) //nolint:gosec // our own paths
	if err != nil {
		return local.Header{}, err
	}
	t, err := local.ParseTable(b)
	if err != nil {
		return local.Header{}, err
	}
	return t.Header, nil
}

func install(src, dst string, log io.Writer) error {
	b, err := os.ReadFile(src) //nolint:gosec // our own cache path
	if err != nil {
		return err
	}
	if err := writeAtomic(dst, b); err != nil {
		return fmt.Errorf("install table: %w", err)
	}
	fmt.Fprintf(log, "embed-table: installed %s (%.1f MB)\n", dst, float64(len(b))/1e6)
	return nil
}

// writeAtomic writes b to path via a hidden temp file in the same directory, so
// a reader (or go:embed, which skips dot-files) never sees a partial file.
func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after a successful rename
	// A build input in the source tree, not a secret: readable like its neighbours.
	if err := tmp.Chmod(0o644); err != nil { //nolint:gosec
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func verifyBytes(b []byte, f pinnedFile) error { return verifyReader(bytes.NewReader(b), f) }

// defaultCacheRoot is ${XDG_CACHE_HOME:-~/.cache}/amplio/localembed.
func defaultCacheRoot() string {
	root := os.Getenv("XDG_CACHE_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "amplio-localembed")
		}
		root = filepath.Join(home, ".cache")
	}
	return filepath.Join(root, "amplio", "localembed")
}

// envOr returns $name, or def when it is unset.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func main() {
	out := flag.String("out", filepath.Join("internal", "embed", "local", "data"), "directory to install "+local.TableFile+" into")
	cacheRoot := flag.String("cache", envOr("AMPLIO_EMBED_CACHE", defaultCacheRoot()),
		"download and table cache root ($AMPLIO_EMBED_CACHE)")
	baseURL := flag.String("base-url", envOr("AMPLIO_EMBED_BASE_URL", "https://huggingface.co"),
		"model mirror root; files are fetched from <base-url>/<repo>/resolve/<revision>/<file> ($AMPLIO_EMBED_BASE_URL)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cfg := config{
		src:      pinned,
		baseURL:  *baseURL,
		cacheDir: filepath.Join(*cacheRoot, pinned.cacheKey()),
		outDir:   *out,
		fetch: fetcher{
			client:   &http.Client{Timeout: 15 * time.Minute},
			attempts: 3,
			backoff:  2 * time.Second,
			log:      os.Stderr,
		},
		log: os.Stderr,
	}
	if err := run(ctx, cfg); err != nil {
		fmt.Fprint(os.Stderr, failureMessage(err, cfg))
		os.Exit(1)
	}
}

// failureMessage is the loud, actionable report for a failed build.
func failureMessage(err error, cfg config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nembed-table: FAILED to build the built-in embedding table.\n  error:     %v\n", err)
	if errors.Is(err, errChecksum) {
		b.WriteString("  The downloaded file does not match the pinned checksum; nothing was installed.\n")
	}
	fmt.Fprintf(&b, "  source:    %s (%s, %s)\n  cache dir: %s\n",
		cfg.src.url(cfg.baseURL, pinnedFile{}), cfg.src.Tokenizer.Name, cfg.src.Weights.Name, cfg.cacheDir)
	b.WriteString("  To build without the table (recall then ranks by BM25 only), run: make build LOCAL_EMBED=0\n")
	return b.String()
}
