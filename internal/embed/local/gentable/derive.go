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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"amplio/internal/embed/local"
)

// parseTokenizer reads a tokenizer.json and returns its vocabulary (by id) and
// the settings the encoder needs. It refuses anything the encoder does not
// implement exactly: a silently different tokenizer would produce plausible
// but wrong vectors.
func parseTokenizer(raw []byte) ([]string, local.TokenizerConfig, error) {
	var cfg local.TokenizerConfig
	var tj struct {
		Model struct {
			Type         string           `json:"type"`
			Unk          string           `json:"unk_token"`
			Prefix       string           `json:"continuing_subword_prefix"`
			MaxWordChars int              `json:"max_input_chars_per_word"`
			Vocab        map[string]int32 `json:"vocab"`
		} `json:"model"`
		Normalizer struct {
			Type         string `json:"type"`
			CleanText    bool   `json:"clean_text"`
			ChineseChars bool   `json:"handle_chinese_chars"`
			StripAccents *bool  `json:"strip_accents"` // null: follows lowercase
			Lowercase    bool   `json:"lowercase"`
		} `json:"normalizer"`
		PreTokenizer struct {
			Type string `json:"type"`
		} `json:"pre_tokenizer"`
		AddedTokens []struct {
			ID         int32  `json:"id"`
			Content    string `json:"content"`
			SingleWord bool   `json:"single_word"`
			Lstrip     bool   `json:"lstrip"`
			Rstrip     bool   `json:"rstrip"`
			Normalized bool   `json:"normalized"`
		} `json:"added_tokens"`
	}
	if err := json.Unmarshal(raw, &tj); err != nil {
		return nil, cfg, fmt.Errorf("tokenizer.json: %w", err)
	}
	n := tj.Normalizer
	switch {
	case tj.Model.Type != "WordPiece":
		return nil, cfg, fmt.Errorf("tokenizer.json: model %q, want WordPiece", tj.Model.Type)
	case n.Type != "BertNormalizer" || !n.CleanText || !n.ChineseChars || !n.Lowercase ||
		(n.StripAccents != nil && !*n.StripAccents):
		return nil, cfg, fmt.Errorf("tokenizer.json: normalizer %+v, want BertNormalizer with every option on", n)
	case tj.PreTokenizer.Type != "BertPreTokenizer":
		return nil, cfg, fmt.Errorf("tokenizer.json: pre_tokenizer %q, want BertPreTokenizer", tj.PreTokenizer.Type)
	case tj.Model.MaxWordChars <= 0:
		return nil, cfg, errors.New("tokenizer.json: max_input_chars_per_word must be positive")
	}
	vocab := make([]string, len(tj.Model.Vocab))
	for tok, id := range tj.Model.Vocab {
		if id < 0 || int(id) >= len(vocab) || vocab[id] != "" {
			return nil, cfg, fmt.Errorf("tokenizer.json: vocabulary ids are not 0..%d", len(vocab)-1)
		}
		vocab[id] = tok
	}
	cfg = local.TokenizerConfig{Unk: tj.Model.Unk, Prefix: tj.Model.Prefix, MaxWordChars: tj.Model.MaxWordChars}
	for _, a := range tj.AddedTokens {
		if a.SingleWord || a.Lstrip || a.Rstrip || a.Normalized {
			return nil, cfg, fmt.Errorf("tokenizer.json: added token %q needs matching options the encoder lacks", a.Content)
		}
		if int(a.ID) >= len(vocab) || a.ID < 0 || vocab[a.ID] != a.Content {
			return nil, cfg, fmt.Errorf("tokenizer.json: added token %q is not vocabulary id %d", a.Content, a.ID)
		}
		cfg.Special = append(cfg.Special, a.Content)
	}
	return vocab, cfg, nil
}

// parseSafetensors returns the single 2-D float32 tensor in a safetensors file.
func parseSafetensors(raw []byte) (matrix, error) {
	if len(raw) < 8 {
		return matrix{}, errors.New("safetensors: truncated")
	}
	hlen := binary.LittleEndian.Uint64(raw)
	if hlen > uint64(len(raw)-8) {
		return matrix{}, errors.New("safetensors: header length past end of file")
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(raw[8:8+hlen], &header); err != nil {
		return matrix{}, fmt.Errorf("safetensors header: %w", err)
	}
	delete(header, "__metadata__")
	if len(header) != 1 {
		return matrix{}, fmt.Errorf("safetensors: %d tensors, want exactly 1", len(header))
	}
	var t struct {
		Dtype   string   `json:"dtype"`
		Shape   []int    `json:"shape"`
		Offsets [2]int64 `json:"data_offsets"`
	}
	for _, v := range header {
		if err := json.Unmarshal(v, &t); err != nil {
			return matrix{}, fmt.Errorf("safetensors tensor: %w", err)
		}
	}
	if t.Dtype != "F32" || len(t.Shape) != 2 || t.Shape[0] <= 0 || t.Shape[1] <= 0 {
		return matrix{}, fmt.Errorf("safetensors: tensor %s%v, want a 2-D F32 matrix", t.Dtype, t.Shape)
	}
	body := raw[8+hlen:]
	size := int64(t.Shape[0]) * int64(t.Shape[1]) * 4
	if t.Offsets[0] != 0 || t.Offsets[1] != size || int64(len(body)) < size {
		return matrix{}, fmt.Errorf("safetensors: data offsets %v do not fit shape %v", t.Offsets, t.Shape)
	}
	m := matrix{rows: t.Shape[0], cols: t.Shape[1], data: make([]float32, t.Shape[0]*t.Shape[1])}
	for i := range m.data {
		m.data[i] = math.Float32frombits(binary.LittleEndian.Uint32(body[4*i:]))
	}
	return m, nil
}

// derive builds the shipped table: PCA to dims, then int8 per-row quantization.
func derive(src source, tokenizerJSON, weights []byte) (*local.Table, error) {
	vocab, tcfg, err := parseTokenizer(tokenizerJSON)
	if err != nil {
		return nil, err
	}
	m, err := parseSafetensors(weights)
	if err != nil {
		return nil, err
	}
	if m.rows != len(vocab) {
		return nil, fmt.Errorf("weights have %d rows for a %d-token vocabulary", m.rows, len(vocab))
	}
	axes, err := principalAxes(m, src.Dims)
	if err != nil {
		return nil, err
	}
	q, scales := quantize(project(m, axes))
	t := &local.Table{
		Header: src.header(len(vocab), tcfg),
		Vocab:  vocab,
		Rows:   q,
		Scales: scales,
	}
	return t, t.Validate()
}
