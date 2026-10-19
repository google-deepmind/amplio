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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// pinnedFile is one upstream file, identified by content.
type pinnedFile struct {
	Name   string
	SHA256 string
	Size   int64
}

// errChecksum marks a download whose content is wrong; retrying cannot help.
var errChecksum = errors.New("checksum mismatch")

// errIncomplete marks a download that ended early; worth retrying.
var errIncomplete = errors.New("incomplete download")

// httpError is a non-200 response.
type httpError struct {
	url    string
	status string
	code   int
}

func (e *httpError) Error() string { return fmt.Sprintf("GET %s: HTTP %s", e.url, e.status) }

func (e *httpError) retryable() bool { return e.code == http.StatusTooManyRequests || e.code >= 500 }

// fetcher downloads pinned files into a cache directory.
type fetcher struct {
	client   *http.Client
	attempts int
	backoff  time.Duration // doubled after each failed attempt
	log      io.Writer
}

// ensure returns the path of f in dir, downloading it from url when the cached
// copy is missing or does not match the pin. A file is only ever moved into
// dir after its size and sha256 check out.
func (fe fetcher) ensure(ctx context.Context, url, dir string, f pinnedFile) (string, error) {
	dst := filepath.Join(dir, f.Name)
	if err := verifyFile(dst, f); err == nil {
		return dst, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(fe.log, "embed-table: cached %s is unusable (%v); downloading again\n", f.Name, err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	delay := fe.backoff
	for attempt := 1; ; attempt++ {
		fmt.Fprintf(fe.log, "embed-table: downloading %s (%.1f MB, attempt %d/%d)\n", url, float64(f.Size)/1e6, attempt, fe.attempts)
		err := fe.download(ctx, url, dst, f)
		if err == nil {
			return dst, nil
		}
		var he *httpError
		if errors.Is(err, errChecksum) || (errors.As(err, &he) && !he.retryable()) || ctx.Err() != nil {
			return "", err
		}
		if attempt >= fe.attempts {
			return "", fmt.Errorf("%w (gave up after %d attempts)", err, attempt)
		}
		fmt.Fprintf(fe.log, "embed-table: %v; retrying in %s\n", err, delay)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// download streams url into a temp file next to dst, then renames it into
// place only if it matches f.
func (fe fetcher) download(ctx context.Context, url, dst string, f pinnedFile) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := fe.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &httpError{url: url, status: resp.Status, code: resp.StatusCode}
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+f.Name+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after a successful rename
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if err := tmp.Close(); err != nil && copyErr == nil {
		copyErr = err
	}
	switch got := hex.EncodeToString(h.Sum(nil)); {
	case copyErr != nil:
		return fmt.Errorf("GET %s: %w after %d bytes: %w", url, errIncomplete, n, copyErr)
	case n < f.Size:
		return fmt.Errorf("GET %s: %w: got %d of %d bytes", url, errIncomplete, n, f.Size)
	case n != f.Size || got != f.SHA256:
		return fmt.Errorf("GET %s: %w: got sha256 %s (%d bytes), want %s (%d bytes)",
			url, errChecksum, got, n, f.SHA256, f.Size)
	}
	return os.Rename(tmp.Name(), dst)
}

// verifyFile checks that path holds exactly the pinned content.
func verifyFile(path string, f pinnedFile) error {
	fh, err := os.Open(path) //nolint:gosec // our own cache path
	if err != nil {
		return err
	}
	defer fh.Close()
	return verifyReader(fh, f)
}

func verifyReader(r io.Reader, f pinnedFile) error {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); n != f.Size || got != f.SHA256 {
		return fmt.Errorf("%w: got sha256 %s (%d bytes), want %s (%d bytes)", errChecksum, got, n, f.SHA256, f.Size)
	}
	return nil
}
