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

package local

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// Table file layout (all integers little-endian):
//
//	magic   8 bytes  "AMPLSE01"
//	hlen    uint32   length of the header JSON
//	header  hlen bytes of JSON (Header)
//	vocab   per token, in id order: uvarint byte length + UTF-8 bytes
//	rows    vocab*dims int8 values, row-major
//	scales  vocab float32 values: row i dequantizes as rows[i]*scales[i]
//
// One self-contained file, so the binary embeds exactly one artifact and the
// tokenizer can never drift from the table it was derived with.
const tableMagic = "AMPLSE01"

// TableFormat is the layout version written into Header.Format.
const TableFormat = 1

// TableFile is the table's file name inside data/ (see the go:embed in
// local.go and the Makefile's embed-table target).
const TableFile = "table.bin"

// maxHeaderLen bounds the header so a corrupt length can't trigger a huge read.
const maxHeaderLen = 1 << 20

// Header describes a table: where it came from, how it was derived, and the
// tokenizer settings the encoder needs.
type Header struct {
	Format    int    `json:"format"`
	Model     string `json:"model"`     // source repo, e.g. minishlab/potion-base-32M
	Revision  string `json:"revision"`  // pinned source revision
	Name      string `json:"name"`      // short display name, e.g. potion-base-32M
	Dims      int    `json:"dims"`      // row width after PCA
	Quant     string `json:"quant"`     // quantization scheme (QuantInt8Row)
	Generator int    `json:"generator"` // derivation recipe version
	Vocab     int    `json:"vocab"`

	Tokenizer TokenizerConfig `json:"tokenizer"`
}

// QuantInt8Row is symmetric per-row int8: scale = max|x|/127, q = round(x/scale).
const QuantInt8Row = "int8-row"

// TokenizerConfig is the part of a WordPiece tokenizer.json the encoder uses;
// the generator refuses any tokenizer whose other settings differ from what
// the encoder implements (see gentable).
type TokenizerConfig struct {
	Unk          string   `json:"unk"`            // unknown-token string, e.g. [UNK]
	Prefix       string   `json:"prefix"`         // continuation prefix, e.g. ##
	MaxWordChars int      `json:"max_word_chars"` // longer words become [UNK]
	Special      []string `json:"special"`        // added tokens, matched verbatim before normalization
}

// Table is a quantized static-embedding table with its vocabulary.
type Table struct {
	Header Header
	Vocab  []string  // token by id
	Rows   []int8    // len(Vocab)*Header.Dims
	Scales []float32 // per row
}

// Validate checks that the sizes agree with each other and with the header.
func (t *Table) Validate() error {
	h := t.Header
	switch {
	case h.Format != TableFormat:
		return fmt.Errorf("table format %d, want %d", h.Format, TableFormat)
	case h.Quant != QuantInt8Row:
		return fmt.Errorf("table quantization %q, want %q", h.Quant, QuantInt8Row)
	case h.Dims <= 0 || h.Vocab <= 0:
		return fmt.Errorf("table shape %dx%d", h.Vocab, h.Dims)
	case len(t.Vocab) != h.Vocab:
		return fmt.Errorf("table has %d tokens, header says %d", len(t.Vocab), h.Vocab)
	case len(t.Rows) != h.Vocab*h.Dims:
		return fmt.Errorf("table has %d values, want %d", len(t.Rows), h.Vocab*h.Dims)
	case len(t.Scales) != h.Vocab:
		return fmt.Errorf("table has %d scales, want %d", len(t.Scales), h.Vocab)
	}
	return nil
}

// Marshal serializes t in the table file layout.
func (t *Table) Marshal() ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	hdr, err := json.Marshal(t.Header)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(tableMagic)+4+len(hdr)+len(t.Rows)+4*len(t.Scales)+8*len(t.Vocab))
	out = append(out, tableMagic...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(hdr))) //nolint:gosec // header is small
	out = append(out, hdr...)
	for _, tok := range t.Vocab {
		out = binary.AppendUvarint(out, uint64(len(tok)))
		out = append(out, tok...)
	}
	for _, v := range t.Rows {
		out = append(out, byte(v)) //nolint:gosec // int8 -> two's-complement byte
	}
	for _, s := range t.Scales {
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(s))
	}
	return out, nil
}

var errTruncated = errors.New("table file truncated")

// ReadHeader parses only the header of a table file.
func ReadHeader(b []byte) (Header, int, error) {
	var h Header
	if len(b) < len(tableMagic)+4 || string(b[:len(tableMagic)]) != tableMagic {
		return h, 0, errors.New("not an amplio embedding table (bad magic)")
	}
	n := int(binary.LittleEndian.Uint32(b[len(tableMagic):]))
	off := len(tableMagic) + 4
	if n > maxHeaderLen || len(b) < off+n {
		return h, 0, errTruncated
	}
	if err := json.Unmarshal(b[off:off+n], &h); err != nil {
		return h, 0, fmt.Errorf("table header: %w", err)
	}
	return h, off + n, nil
}

// ParseTable decodes and validates a table file. The result does not alias b.
func ParseTable(b []byte) (*Table, error) {
	h, off, err := ReadHeader(b)
	if err != nil {
		return nil, err
	}
	if h.Vocab <= 0 || h.Dims <= 0 || h.Vocab > len(b) || h.Dims > len(b) {
		return nil, fmt.Errorf("table shape %dx%d", h.Vocab, h.Dims)
	}
	t := &Table{Header: h, Vocab: make([]string, h.Vocab)}
	for i := range t.Vocab {
		n, k := binary.Uvarint(b[off:])
		if k <= 0 || n > uint64(len(b)-off-k) {
			return nil, errTruncated
		}
		off += k
		t.Vocab[i] = string(b[off : off+int(n)])
		off += int(n)
	}
	nv := h.Vocab * h.Dims
	if len(b)-off != nv+4*h.Vocab {
		return nil, fmt.Errorf("table body is %d bytes, want %d", len(b)-off, nv+4*h.Vocab)
	}
	t.Rows = make([]int8, nv)
	for i, v := range b[off : off+nv] {
		t.Rows[i] = int8(v) //nolint:gosec // two's-complement byte -> int8
	}
	off += nv
	t.Scales = make([]float32, h.Vocab)
	for i := range t.Scales {
		t.Scales[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[off+4*i:]))
	}
	return t, t.Validate()
}
