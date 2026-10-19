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

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"amplio/internal/embed/local"
)

func TestSymEigen(t *testing.T) {
	const n = 12
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test data
	a := make([]float64, n*n)
	for i := range n {
		for j := i; j < n; j++ {
			v := rng.NormFloat64()
			a[i*n+j], a[j*n+i] = v, v
		}
	}
	vals, vecs, err := symEigen(a, n)
	if err != nil {
		t.Fatal(err)
	}
	for c := range n {
		// A v = lambda v, and the columns are orthonormal.
		for i := range n {
			var av float64
			for j := range n {
				av += a[i*n+j] * vecs[j*n+c]
			}
			if math.Abs(av-vals[c]*vecs[i*n+c]) > 1e-9 {
				t.Fatalf("column %d is not an eigenvector (row %d: %v vs %v)", c, i, av, vals[c]*vecs[i*n+c])
			}
		}
		for c2 := range n {
			var dot float64
			for i := range n {
				dot += vecs[i*n+c] * vecs[i*n+c2]
			}
			want := 0.0
			if c == c2 {
				want = 1
			}
			if math.Abs(dot-want) > 1e-9 {
				t.Fatalf("columns %d, %d: dot %v, want %v", c, c2, dot, want)
			}
		}
	}
}

// Rows spread along known orthogonal axes with distinct variances: PCA must
// recover the axes in variance order, each sign-fixed.
func TestPrincipalAxes(t *testing.T) {
	true1 := []float64{-0.8, 0.6, 0, 0} // largest component negative: the axis must come out flipped
	true2 := []float64{0, 0, 1, 0}
	rng := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // deterministic test data
	const rows = 4000
	m := matrix{rows: rows, cols: 4, data: make([]float32, rows*4)}
	for i := range rows {
		a, b := 5*rng.NormFloat64(), 2*rng.NormFloat64()
		for j := range 4 {
			m.data[i*4+j] = float32(a*true1[j] + b*true2[j] + 0.01*rng.NormFloat64() + 3) // + offset: centering matters
		}
	}
	axes, err := principalAxes(m, 2)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(axes[0][0]-0.8) > 1e-3 || math.Abs(axes[0][1]+0.6) > 1e-3 {
		t.Errorf("first axis %v, want (0.8, -0.6, 0, 0)", axes[0])
	}
	if math.Abs(axes[1][2]-1) > 1e-3 {
		t.Errorf("second axis %v, want %v", axes[1], true2)
	}
	for i, ax := range axes {
		best := slices.MaxFunc(ax, func(x, y float64) int { return cmpAbs(x, y) })
		if best < 0 {
			t.Errorf("axis %d %v: largest-magnitude component must be positive", i, ax)
		}
	}
	if _, err := principalAxes(m, 5); err == nil {
		t.Error("more axes than columns must be an error")
	}

	// Projection uses the raw rows: a row is mapped to its dot with each axis.
	p := project(matrix{rows: 1, cols: 4, data: []float32{1, 2, 3, 4}}, axes)
	for a, ax := range axes {
		want := ax[0] + 2*ax[1] + 3*ax[2] + 4*ax[3]
		if math.Abs(float64(p.data[a])-want) > 1e-5 {
			t.Errorf("projection[%d] = %v, want %v", a, p.data[a], want)
		}
	}
}

func cmpAbs(x, y float64) int {
	switch ax, ay := math.Abs(x), math.Abs(y); {
	case ax < ay:
		return -1
	case ax > ay:
		return 1
	}
	return 0
}

func TestQuantize(t *testing.T) {
	q, scales := quantize(matrix{rows: 3, cols: 3, data: []float32{
		127, -254, 63.5, // scale 2: 63.5 -> -127 .. 31.75
		254, 3, 5, // scale 2: 1.5 and 2.5 round half to even -> 2, 2
		0, 0, 0, // all-zero row: scale 1
	}})
	if want := []int8{64, -127, 32, 127, 2, 2, 0, 0, 0}; !slices.Equal(q, want) {
		t.Errorf("q = %v, want %v", q, want)
	}
	if want := []float32{2, 2, 1}; !slices.Equal(scales, want) {
		t.Errorf("scales = %v, want %v", scales, want)
	}
}

func pin(name string, b []byte) pinnedFile {
	sum := sha256.Sum256(b)
	return pinnedFile{Name: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))}
}

func testFetcher() fetcher {
	return fetcher{client: http.DefaultClient, attempts: 3, backoff: time.Millisecond, log: io.Discard}
}

func TestFetch(t *testing.T) {
	good := []byte("the pinned content")
	f := pin("f.bin", good)
	var hits atomic.Int32
	var body func(w http.ResponseWriter)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		body(w)
	}))
	defer srv.Close()
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		body     func(w http.ResponseWriter)
		wantErr  error // nil: success
		wantHits int32
	}{
		{"ok", func(w http.ResponseWriter) { _, _ = w.Write(good) }, nil, 1},
		// Same size, different bytes: a corrupt or tampered file. Not retried.
		{"checksum mismatch", func(w http.ResponseWriter) { _, _ = w.Write(bytes.ToUpper(good)) }, errChecksum, 1},
		// The connection drops mid-body: retried, then reported.
		{"partial", func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write(good[:5])
		}, errIncomplete, 3},
		{"short body", func(w http.ResponseWriter) { _, _ = w.Write(good[:5]) }, errIncomplete, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			hits.Store(0)
			body = tc.body
			path, err := testFetcher().ensure(ctx, srv.URL+"/f.bin", dir, f)
			if hits.Load() != tc.wantHits {
				t.Errorf("%d requests, want %d", hits.Load(), tc.wantHits)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatal(err)
				}
				if got, _ := os.ReadFile(path); !bytes.Equal(got, good) { //nolint:gosec // test temp dir
					t.Errorf("cached file = %q", got)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("a failed download left files behind: %v", entries)
			}
		})
	}

	t.Run("HTTP status", func(t *testing.T) {
		for code, attempts := range map[int]int32{http.StatusNotFound: 1, http.StatusBadGateway: 3} {
			hits.Store(0)
			body = func(w http.ResponseWriter) { w.WriteHeader(code) }
			_, err := testFetcher().ensure(ctx, srv.URL+"/f.bin", t.TempDir(), f)
			var he *httpError
			if !errors.As(err, &he) || he.code != code || !strings.Contains(err.Error(), srv.URL) {
				t.Errorf("HTTP %d: err = %v, want an httpError naming the URL", code, err)
			}
			if hits.Load() != attempts {
				t.Errorf("HTTP %d: %d attempts, want %d", code, hits.Load(), attempts)
			}
		}
	})

	t.Run("verified cache is reused, corrupt cache replaced", func(t *testing.T) {
		dir := t.TempDir()
		body = func(w http.ResponseWriter) { _, _ = w.Write(good) }
		if err := os.WriteFile(filepath.Join(dir, f.Name), []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
		hits.Store(0)
		if _, err := testFetcher().ensure(ctx, srv.URL+"/f.bin", dir, f); err != nil || hits.Load() != 1 {
			t.Fatalf("corrupt cache: err=%v requests=%d, want a fresh download", err, hits.Load())
		}
		if _, err := testFetcher().ensure(ctx, srv.URL+"/f.bin", dir, f); err != nil || hits.Load() != 1 {
			t.Fatalf("valid cache: err=%v requests=%d, want no download", err, hits.Load())
		}
	})
}

// fakeModel is a tiny WordPiece tokenizer.json plus a matching safetensors
// file: enough for the whole pipeline to run on test data.
func fakeModel(t *testing.T, vocab []string, cols int) (tokJSON, weights []byte) {
	t.Helper()
	v := map[string]int{}
	for i, tok := range vocab {
		v[tok] = i
	}
	tj := map[string]any{
		"normalizer":    map[string]any{"type": "BertNormalizer", "clean_text": true, "handle_chinese_chars": true, "strip_accents": nil, "lowercase": true},
		"pre_tokenizer": map[string]any{"type": "BertPreTokenizer"},
		"model":         map[string]any{"type": "WordPiece", "unk_token": "[UNK]", "continuing_subword_prefix": "##", "max_input_chars_per_word": 100, "vocab": v},
		"added_tokens": []map[string]any{{"id": 0, "content": "[PAD]", "special": true},
			{"id": 1, "content": "[UNK]", "special": true}},
	}
	tokJSON, err := json.Marshal(tj)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // deterministic test data
	data := make([]byte, 0, len(vocab)*cols*4)
	for range len(vocab) * cols {
		data = binary.LittleEndian.AppendUint32(data, math.Float32bits(float32(rng.NormFloat64())))
	}
	hdr, err := json.Marshal(map[string]any{
		"__metadata__": map[string]string{"format": "pt"},
		"embeddings":   map[string]any{"dtype": "F32", "shape": []int{len(vocab), cols}, "data_offsets": []int{0, len(data)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	weights = binary.LittleEndian.AppendUint64(nil, uint64(len(hdr)))
	weights = append(append(weights, hdr...), data...)
	return tokJSON, weights
}

// mirror serves files at <root>/<repo>/resolve/<revision>/<name> and counts requests.
type mirror struct {
	*httptest.Server
	files map[string][]byte
	hits  atomic.Int32
}

func newMirror(t *testing.T, src source, files map[string][]byte) *mirror {
	m := &mirror{files: files}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		name, ok := strings.CutPrefix(r.URL.Path, "/"+src.Repo+"/resolve/"+src.Revision+"/")
		b, found := m.files[name]
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(m.Close)
	return m
}

func testSource(tokJSON, weights []byte) source {
	return source{
		Repo: "test/tiny", Revision: "abc123", Name: "tiny", Dims: 3,
		Tokenizer: pin("tokenizer.json", tokJSON), Weights: pin("model.safetensors", weights),
	}
}

func testConfig(t *testing.T, src source, baseURL string) config {
	t.Helper()
	root := t.TempDir()
	return config{
		src: src, baseURL: baseURL, cacheDir: filepath.Join(root, "cache", src.cacheKey()),
		outDir: filepath.Join(root, "data"), fetch: testFetcher(), log: io.Discard,
	}
}

func TestRun_DeriveInstallAndReuse(t *testing.T) {
	vocab := []string{"[PAD]", "[UNK]", "hello", "world", "##s", "run", "!", "a"}
	tokJSON, weights := fakeModel(t, vocab, 6)
	src := testSource(tokJSON, weights)
	mir := newMirror(t, src, map[string][]byte{"tokenizer.json": tokJSON, "model.safetensors": weights})
	cfg := testConfig(t, src, mir.URL)
	ctx := context.Background()

	if err := run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(cfg.outDir, local.TableFile)
	b, err := os.ReadFile(installed) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := local.ParseTable(b)
	if err != nil {
		t.Fatal(err)
	}
	if h := tbl.Header; !src.matches(h) || h.Vocab != len(vocab) || h.Dims != 3 ||
		!slices.Equal(tbl.Vocab, vocab) || !slices.Equal(h.Tokenizer.Special, []string{"[PAD]", "[UNK]"}) {
		t.Fatalf("installed table header %+v / vocab %v", h, tbl.Vocab)
	}
	if mir.hits.Load() != 2 {
		t.Errorf("%d downloads, want 2", mir.hits.Load())
	}

	// Up to date: a no-op without touching the network.
	if err := run(ctx, cfg); err != nil || mir.hits.Load() != 2 {
		t.Fatalf("rerun: err=%v downloads=%d, want a no-op", err, mir.hits.Load())
	}
	// Installed table removed (or corrupt): reinstalled from the derived-table cache.
	if err := os.WriteFile(installed, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, cfg); err != nil || mir.hits.Load() != 2 {
		t.Fatalf("reinstall: err=%v downloads=%d, want the cached table", err, mir.hits.Load())
	}
	if again, _ := os.ReadFile(installed); !bytes.Equal(again, b) { //nolint:gosec // test temp dir
		t.Error("reinstalled table differs from the first one")
	}
	// A new generator recipe (here: a different width) rebuilds from the
	// cached downloads, still without the network.
	cfg.src.Dims = 2
	if err := run(ctx, cfg); err != nil || mir.hits.Load() != 2 {
		t.Fatalf("re-derive: err=%v downloads=%d", err, mir.hits.Load())
	}
	if h, err := readTableHeader(installed); err != nil || h.Dims != 2 {
		t.Fatalf("re-derived header %+v, err %v", h, err)
	}
}

func TestRun_ChecksumMismatchInstallsNothing(t *testing.T) {
	vocab := []string{"[PAD]", "[UNK]", "hello", "world"}
	tokJSON, weights := fakeModel(t, vocab, 4)
	src := testSource(tokJSON, weights)
	tampered := append([]byte(nil), weights...)
	tampered[len(tampered)-1] ^= 1
	mir := newMirror(t, src, map[string][]byte{"tokenizer.json": tokJSON, "model.safetensors": tampered})
	cfg := testConfig(t, src, mir.URL)

	err := run(context.Background(), cfg)
	if !errors.Is(err, errChecksum) {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(cfg.outDir, local.TableFile)); !os.IsNotExist(statErr) {
		t.Error("a table was installed despite the checksum mismatch")
	}
	if _, statErr := os.Stat(filepath.Join(cfg.cacheDir, "model.safetensors")); !os.IsNotExist(statErr) {
		t.Error("the mismatching download was kept in the cache")
	}
	msg := failureMessage(err, cfg)
	for _, want := range []string{"checksum", mir.URL, cfg.cacheDir, "make build LOCAL_EMBED=0", "nothing was installed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message lacks %q:\n%s", want, msg)
		}
	}
}

func TestParseTokenizer_RejectsUnsupported(t *testing.T) {
	tokJSON, _ := fakeModel(t, []string{"[PAD]", "[UNK]", "x"}, 2)
	if _, _, err := parseTokenizer(tokJSON); err != nil {
		t.Fatalf("baseline tokenizer rejected: %v", err)
	}
	for name, edit := range map[string]func(m map[string]any){
		"BPE model":          func(m map[string]any) { m["model"].(map[string]any)["type"] = "BPE" },
		"no lowercase":       func(m map[string]any) { m["normalizer"].(map[string]any)["lowercase"] = false },
		"accents kept":       func(m map[string]any) { m["normalizer"].(map[string]any)["strip_accents"] = false },
		"other pretokenizer": func(m map[string]any) { m["pre_tokenizer"].(map[string]any)["type"] = "Whitespace" },
		"lstrip added token": func(m map[string]any) {
			m["added_tokens"] = []map[string]any{{"id": 0, "content": "[PAD]", "lstrip": true}}
		},
	} {
		var m map[string]any
		if err := json.Unmarshal(tokJSON, &m); err != nil {
			t.Fatal(err)
		}
		edit(m)
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := parseTokenizer(b); err == nil {
			t.Errorf("%s: accepted a tokenizer the encoder does not implement", name)
		}
	}
}
