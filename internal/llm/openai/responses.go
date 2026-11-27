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

package openai

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"amplio/internal/llm"
)

// ResponsesClientArgs are the client-block arguments of openai-responses.
var ResponsesClientArgs = map[string]bool{
	"base_url":    true,
	"api_key_env": true,
	"profile":     true,
	"webp":        true,
	"call_stream": true,
	"tool_strict": true,
}

const (
	minResponsesMaxOutputTokens = 16
	responsesChainKey           = "openai.responses_chain"
	responsesSSEMaxLine         = 32 << 20
)

type responsesProvider struct {
	model      string
	maxTokens  int
	url        string
	apiKey     string
	http       *http.Client
	extra      map[string]any
	hosted     bool
	webp       bool
	callStream bool
	toolStrict bool

	// URL, effective model, credentials and routing options are fixed for
	// this instance. Never share learned restrictions across configurations.
	// Unknown and positively stateful backends need the same request policy.
	replayOnly atomic.Bool // contract, explicit opt-out, or observed rejection
	missingIDs sync.Map    // explicitly rejected ID -> struct{}
}

// NewResponses builds a provider targeting the native Responses endpoint.
func NewResponses(model string, maxTokens int, clientArgs, args url.Values) (llm.Provider, error) {
	profile := clientArgs.Get("profile")
	base := strings.TrimRight(cmp.Or(clientArgs.Get("base_url"), os.Getenv("OPENAI_BASE_URL"), defaultBaseURL), "/")
	if _, err := url.Parse(base); err != nil {
		return nil, err
	}
	if profile == "" {
		switch base {
		case defaultBaseURL:
			profile = "openai"
		default:
			profile = "generic"
		}
	}
	switch profile {
	case "generic", "litellm", "ollama", "openai", "vllm":
	default:
		return nil, fmt.Errorf("unknown profile %q; known: generic, litellm, ollama, openai, vllm", profile)
	}

	hosted := profile == "openai"
	webp, err := llm.WebPArg(clientArgs, hosted || profile == "vllm")
	if err != nil {
		return nil, err
	}
	extra, err := expandArgs(args)
	if err != nil {
		return nil, err
	}
	callStream, err := parseBoolArg(clientArgs, "call_stream")
	if err != nil {
		return nil, err
	}
	toolStrict, err := parseBoolArg(clientArgs, "tool_strict")
	if err != nil {
		return nil, err
	}
	maxTokens, err = prepareResponsesArgs(extra, maxTokens)
	if err != nil {
		return nil, err
	}

	p := &responsesProvider{
		model:      model,
		maxTokens:  maxTokens,
		url:        base + "/responses",
		apiKey:     os.Getenv(cmp.Or(clientArgs.Get("api_key_env"), "OPENAI_API_KEY")),
		http:       &http.Client{Timeout: requestTimeout},
		extra:      extra,
		hosted:     hosted,
		webp:       webp,
		callStream: callStream,
		toolStrict: toolStrict,
	}
	p.replayOnly.Store(extra["store"] == false)
	return p, nil
}

func (p *responsesProvider) ModelID() string { return p.model }
func (p *responsesProvider) MaxTokens() int  { return p.maxTokens }

func parseBoolArg(v url.Values, key string) (bool, error) {
	s := v.Get(key)
	if s == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("%s=%q: %w", key, s, err)
	}
	return b, nil
}

// Consume provider settings here. What remains in extra has request-body
// precedence and is read-only for the lifetime of the provider.
func prepareResponsesArgs(extra map[string]any, fallback int) (int, error) {
	for _, key := range []string{"input", "previous_response_id"} {
		if _, ok := extra[key]; ok {
			return 0, fmt.Errorf("openai responses: %s is managed by the provider", key)
		}
	}
	if _, ok := extra["conversation"]; ok {
		return 0, fmt.Errorf("openai responses: conversation is incompatible with previous_response_id")
	}
	if v, ok := extra["background"]; ok && v != false {
		return 0, fmt.Errorf("openai responses: background execution does not return the required terminal response")
	}
	if v, ok := extra["store"]; ok {
		b, ok := v.(bool)
		if !ok {
			return 0, fmt.Errorf("openai responses: store must be a boolean")
		}
		// true requests the normal policy: prefer a stored response where
		// supported. An explicit false, unlike an inferred lack of support,
		// must be forwarded even to a server whose default is to store.
		if b {
			delete(extra, "store")
		}
	}
	if _, ok := extra["strict"]; ok {
		return 0, fmt.Errorf("openai responses: use tool_strict in the client block instead of query argument strict")
	}
	if _, ok := extra["stream"]; ok {
		return 0, fmt.Errorf("openai responses: use call_stream in the client block instead of query argument stream")
	}

	v, ok := extra["max_output_tokens"]
	if !ok {
		return fallback, checkOutputTokenFloor("max_tokens", fallback)
	}
	n, ok := v.(int64)
	if !ok || n < minResponsesMaxOutputTokens || int64(int(n)) != n {
		return 0, fmt.Errorf("openai responses: max_output_tokens=%v must be an integer and the minimum is %d", v, minResponsesMaxOutputTokens)
	}
	delete(extra, "max_output_tokens")
	return int(n), nil
}

func checkOutputTokenFloor(field string, n int) error {
	if n <= 0 || n >= minResponsesMaxOutputTokens {
		return nil
	}
	if field == "max_tokens" {
		return fmt.Errorf("openai responses: %s=%d; minimum max_output_tokens is %d", field, n, minResponsesMaxOutputTokens)
	}
	return fmt.Errorf("openai responses: %s=%d; minimum is %d", field, n, minResponsesMaxOutputTokens)
}

func (p *responsesProvider) requestOptions(req llm.Request, stream bool) (map[string]any, error) {
	limit := req.MaxTokens
	if limit <= 0 {
		limit = p.maxTokens
	}
	if err := checkOutputTokenFloor("max_output_tokens", limit); err != nil {
		return nil, err
	}

	body := map[string]any{"model": p.model}
	if limit > 0 {
		body["max_output_tokens"] = limit
	}
	if req.SystemPrompt != "" {
		body["instructions"] = req.SystemPrompt
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.Tools) != 0 {
		body["tools"] = convertResponsesTools(req.Tools, p.toolStrict)
	}
	if stream {
		body["stream"] = true
	}
	for k, v := range p.extra {
		if stream || k != "stream_options" {
			body[k] = v
		}
	}
	if req.DisableToolCalls && body["tools"] != nil {
		body["tool_choice"] = "none"
	}
	return body, nil
}

func convertResponsesTools(tools []llm.ToolDef, strict bool) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		fn := map[string]any{"type": "function", "name": t.Name, "strict": strict}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(t.Schema) != 0 {
			// A replacement POST can happen after Stream has returned.
			fn["parameters"] = json.RawMessage(bytes.Clone(t.Schema))
		}
		out = append(out, fn)
	}
	return out
}

// The digest and the replay source have different lifetimes. Finishing a
// response needs the digest even when retaining the input is no longer
// necessary (or replay is no longer admissible).
type responsesPrefix struct {
	endpoint string
	digest   hash.Hash
	count    int
	parent   string // last accepted receipt, not necessarily a usable server ID
}

func (h *responsesPrefix) include(m llm.Message) error {
	// Keep the logical digest independent of transport and compatible with
	// persisted ID-only anchors. Opaque protocol history is bound separately.
	if err := json.NewEncoder(h.digest).Encode(m); err != nil {
		return fmt.Errorf("openai responses: identify conversation prefix: %w", err)
	}
	h.count++
	return nil
}

func (h *responsesPrefix) boundary() string {
	return fmt.Sprintf("%d:%x", h.count, h.digest.Sum(nil))
}

func retainedResponsesMessage(m llm.Message) llm.Message {
	m.ProviderExtra = nil
	// Besides taking ownership, normalize empty slices for histories that
	// have passed through a store which does not distinguish [] from nil.
	m.ToolCalls = append([]llm.ToolCall(nil), m.ToolCalls...)
	m.Attachments = append([]llm.Attachment(nil), m.Attachments...)
	return m
}

type responsesReceipt struct {
	id     string // candidate continuation, not a claim that it still exists
	output string // JSON array; "" means no recorded representation, not []
}

// Two turns can project to identical llm.Messages without representing the
// same wire history. Include the preceding receipt and this turn's evidence
// in the stamp. This is a consistency check, not an authentication mechanism.
func (h *responsesPrefix) stamp(r responsesReceipt) string {
	d := sha256.New()
	fmt.Fprintf(d, "%s/%s/%s/%d:", h.endpoint, h.boundary(), h.parent, len(r.id))
	_, _ = io.WriteString(d, r.id)
	_, _ = io.WriteString(d, r.output)
	return fmt.Sprintf("%x", d.Sum(nil))
}

// Called after including the assistant message. An edited prefix, foreign
// endpoint or mismatched branch has no protocol evidence at this position.
func (h *responsesPrefix) read(m llm.Message) responsesReceipt {
	a, _ := m.ProviderExtra[responsesChainKey].(map[string]any)
	if a["endpoint"] != h.endpoint {
		return responsesReceipt{}
	}
	id, _ := a["id"].(string)
	if strings.TrimSpace(id) == "" {
		id = ""
	}
	if v, exists := a["output"]; exists {
		output, ok := v.(string)
		if ok && output != "" {
			r := responsesReceipt{id: id, output: output}
			tag := "v2:" + h.stamp(r)
			if a["prefix"] == tag {
				h.parent = tag
				return r
			}
		}
		return responsesReceipt{}
	}

	// Old writers knew only the logical prefix. Accept their anchors in
	// old histories, but don't use one to jump over a rich-history receipt
	// whose lineage that writer could not have verified.
	if id != "" && !strings.HasPrefix(h.parent, "v2:") && a["prefix"] == h.boundary() {
		r := responsesReceipt{id: id}
		h.parent = "v1:" + h.stamp(r)
		return r
	}
	return responsesReceipt{}
}

func (h *responsesPrefix) seal(r responsesReceipt, out *llm.Response) error {
	m := llm.Message{Role: llm.RoleAssistant, Content: out.Content, ToolCalls: out.ToolCalls}
	if err := h.include(retainedResponsesMessage(m)); err != nil {
		return err
	}
	meta := map[string]any{
		"endpoint": h.endpoint, "prefix": "v2:" + h.stamp(r), "output": r.output,
	}
	if r.id != "" {
		meta["id"] = r.id
	}
	out.ProviderExtra = map[string]any{responsesChainKey: meta}
	return nil
}

// A record substitutes an array (possibly empty) for one projected message.
// Keep the substitutions in history order. Don't decode a large recorded
// prefix just to send a short continuation suffix.
type responsesSubstitution struct {
	at     int
	output string
}

type responsesRequest struct {
	options  map[string]any
	messages []llm.Message
	recorded []responsesSubstitution
}

// The zero plan is full explicit input with no request for persistence.
// Only a stored attempt may have a nonempty ID and a nonzero suffix offset.
type responsesPlan struct {
	store      bool
	previousID string
	from       int
}

func responsesMessage(role llm.Role, content any) map[string]any {
	return map[string]any{"type": "message", "role": role, "content": content}
}

func responsesContent(text string, attachments []llm.Attachment) any {
	if len(attachments) == 0 {
		return text
	}
	parts := make([]any, 0, 1+len(attachments))
	if text != "" {
		parts = append(parts, map[string]any{"type": "input_text", "text": text})
	}
	for _, a := range attachments {
		parts = append(parts, map[string]any{
			"type":      "input_image",
			"image_url": "data:" + a.MimeType + ";base64," + a.Base64Data,
			"detail":    "auto",
		})
	}
	return parts
}

func (d *responsesRequest) input(p *responsesProvider, from int) ([]any, error) {
	input := make([]any, 0, len(d.messages)-from)
	for _, record := range d.recorded {
		if record.at < from {
			continue
		}
		var err error
		input, err = p.appendResponsesInput(input, d.messages[from:record.at])
		if err != nil {
			return nil, err
		}
		var items []json.RawMessage
		if err := json.Unmarshal([]byte(record.output), &items); err != nil {
			return nil, fmt.Errorf("openai responses: decode retained output: %w", err)
		}
		for _, raw := range items {
			input = append(input, raw)
		}
		from = record.at + 1
	}
	return p.appendResponsesInput(input, d.messages[from:])
}

// Used only for locally represented history. In particular, don't transcode
// images, normalize arguments or synthesize assistant text in recorded items.
func (p *responsesProvider) appendResponsesInput(input []any, msgs []llm.Message) ([]any, error) {
	if !p.webp {
		msgs = llm.WithoutWebP(msgs)
	}
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			input = append(input, responsesMessage(m.Role, m.Content))

		case llm.RoleUser:
			input = append(input, responsesMessage(m.Role, responsesContent(m.Content, m.Attachments)))

		case llm.RoleAssistant:
			if m.Content != "" || len(m.ToolCalls) == 0 {
				input = append(input, responsesMessage(m.Role, m.Content))
			}
			for _, tc := range m.ToolCalls {
				if tc.ID == "" {
					return nil, fmt.Errorf("openai responses: historical tool call %q is missing call_id", tc.Name)
				}
				args := tc.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				input = append(input, map[string]any{
					"type": "function_call", "call_id": tc.ID, "name": tc.Name, "arguments": args,
				})
			}

		case llm.RoleToolResult:
			if m.ToolCallID == "" {
				return nil, fmt.Errorf("openai responses: tool result is missing call_id")
			}
			var output any = m.Content
			if p.hosted {
				output = responsesContent(m.Content, m.Attachments)
			}
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": m.ToolCallID, "output": output,
			})
			if !p.hosted && len(m.Attachments) != 0 {
				input = append(input, responsesMessage(llm.RoleUser, responsesContent("", m.Attachments)))
			}
		}
	}
	return input, nil
}

// Failure diagnostics have different envelopes in HTTP, a failed response,
// and a stream error event. Continuation recovery examines the diagnosis,
// never the formatting of the Go error that carries it.
type responsesErrorDetails struct {
	Code    json.RawMessage `json:"code"`
	Param   json.RawMessage `json:"param"`
	Message string          `json:"message"`
}

type responseError struct {
	status  string
	kind    string
	code    string
	param   string
	message string
}

func rawJSONScalar(raw json.RawMessage) string {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return value
}

func (d responsesErrorDetails) failure(status string) *responseError {
	return &responseError{
		status: status, code: rawJSONScalar(d.Code), param: rawJSONScalar(d.Param), message: d.Message,
	}
}

func (e *responseError) Error() string {
	var details []string
	if e.status != "" && e.status != "failed" {
		details = append(details, "status="+e.status)
	}
	if e.kind != "" {
		details = append(details, "type="+e.kind)
	}
	if e.code != "" {
		details = append(details, "code="+e.code)
	}
	if e.param != "" {
		details = append(details, "param="+e.param)
	}
	msg := "openai responses failed"
	if len(details) != 0 {
		msg += " (" + strings.Join(details, ", ") + ")"
	}
	if e.message != "" {
		msg += ": " + e.message
	}
	return msg
}

func (e *responseError) missingPrevious(id string) bool {
	if id == "" || (e.param != "" && e.param != "previous_response_id") {
		return false
	}
	switch e.code {
	case "previous_response_not_found", "previous_response_id_not_found":
		return true
	case "not_found", "resource_not_found", "response_not_found":
		if e.param == "previous_response_id" {
			return true
		}
	}

	// Some compatible servers have only a message. Require a diagnosis
	// about the previous response, not "not found" somewhere in prose.
	msg := strings.TrimSpace(e.message)
	sentence, _, _ := strings.Cut(strings.ToLower(msg), ".")
	switch sentence {
	case "previous response not found", "previous response does not exist":
		return true
	case "not found", "response not found", "response does not exist":
		return e.param == "previous_response_id"
	}
	before, after, found := strings.Cut(msg, id)
	if !found {
		return false
	}
	switch strings.TrimRight(strings.ToLower(before), " '\"`") {
	case "previous response with id", "response with id", "previous response", "response", "previous_response_id":
		diagnosis, _, _ := strings.Cut(strings.TrimLeft(strings.ToLower(after), " '\"`"), ".")
		return diagnosis == "not found" || diagnosis == "was not found" || diagnosis == "does not exist"
	}
	return false
}

// A resource diagnosis and a parameter diagnosis have different scopes.
// In particular, a bad *value* for previous_response_id need not mean that
// the parameter is unsupported.
type responsesRejection uint8

const (
	responsesOtherFailure responsesRejection = iota
	responsesMissingContinuation
	responsesUnsupportedState
)

func (plan responsesPlan) rejection(e *responseError) responsesRejection {
	if !plan.store {
		return responsesOtherFailure
	}
	if e.missingPrevious(plan.previousID) {
		return responsesMissingContinuation
	}
	if e.rejectsParameter("store") || (plan.previousID != "" && e.rejectsParameter("previous_response_id")) {
		return responsesUnsupportedState
	}
	return responsesOtherFailure
}

func (e *responseError) rejectsParameter(field string) bool {
	if e.param != "" && e.param != field {
		return false
	}
	if e.code == field+"_not_supported" || e.code == "unsupported_"+field {
		return true
	}
	if e.param == field {
		switch e.code {
		case "unsupported_parameter", "unknown_parameter", "unrecognized_parameter", "not_supported", "not_implemented":
			return true
		case "unsupported_value":
			// The only store value on a stateful attempt is true.
			return field == "store"
		}
	}

	// Some adapters report only an invalid_request_error and a sentence.
	// Match a diagnosis about the actual field, not two independent keyword
	// searches. E.g. "tool X not supported when store is true" isn't one.
	msg, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(e.message)), ".")
	msg = strings.NewReplacer("'", "", "\"", "", "`", "").Replace(msg)
	msg = strings.Join(strings.Fields(msg), " ")
	for _, scope := range []string{" by this provider", " by this endpoint", " on this endpoint", " with this model", " for this model"} {
		msg = strings.TrimSuffix(msg, scope)
	}
	if e.param == field {
		switch msg {
		case "unsupported parameter", "unknown parameter", "not supported", "not implemented":
			return true
		case "must be false", "only false is supported":
			return field == "store"
		}
	}
	for _, label := range []string{"unknown parameter: ", "unsupported parameter: ", "unrecognized request argument supplied: "} {
		if rest, ok := strings.CutPrefix(msg, label); ok {
			if rest == field {
				return true
			}
			msg = rest
			break
		}
	}
	msg = strings.TrimPrefix(msg, "the ")
	subjects := []string{field, field + " parameter"}
	if field == "store" {
		if msg == "store must be false" || msg == "store must be set to false" {
			return true
		}
		subjects = append(subjects, "store=true", "store: true", "response storage", "storing responses")
	} else {
		subjects = append(subjects, "response chaining")
	}
	for _, s := range subjects {
		switch msg {
		case s + " is not supported", s + " is unsupported", s + " is not implemented",
			"this endpoint does not support " + s, "this provider does not support " + s,
			"this endpoint does not support the " + s, "this provider does not support the " + s:
			return true
		}
	}
	return false
}

func responsesPostError(resp *http.Response, plan responsesPlan) error {
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity:
		if plan.store {
			var envelope struct {
				Error responsesErrorDetails `json:"error"`
			}
			var consumed bytes.Buffer
			probe := io.TeeReader(io.LimitReader(resp.Body, 1<<20), &consumed)
			if json.NewDecoder(probe).Decode(&envelope) == nil {
				if e := envelope.Error.failure("error"); plan.rejection(e) != responsesOtherFailure {
					return e
				}
			}
			// The ordinary HTTP error interpreter must still see the body
			// from byte zero. post owns and closes the original closer.
			resp.Body = io.NopCloser(io.MultiReader(&consumed, resp.Body))
		}
	}
	return httpError(resp)
}

func (d *responsesRequest) post(ctx context.Context, p *responsesProvider, plan responsesPlan) (*http.Response, error) {
	input, err := d.input(p, plan.from)
	if err != nil {
		return nil, err
	}
	body := maps.Clone(d.options)
	body["input"] = input
	if plan.store {
		body["store"] = true
	}
	if plan.previousID != "" {
		body["previous_response_id"] = plan.previousID
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai responses: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if body["stream"] == true {
		req.Header.Set("Accept", "text/event-stream")
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, responsesPostError(resp, plan)
	}
	return resp, nil
}

// A terminal snapshot has a single interpreter. In particular, check status
// before interpreting output: a failed response is not a malformed success
// just because its partial output cannot be projected.
type responsesObject struct {
	ID                string                 `json:"id"`
	Status            string                 `json:"status"`
	Store             *bool                  `json:"store"`
	Error             *responsesErrorDetails `json:"error"`
	ErrorType         string                 `json:"error_type"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output json.RawMessage `json:"output"`
	Usage  struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		TotalTokens        int `json:"total_tokens"`
		InputTokensDetails struct {
			CachedTokens     int `json:"cached_tokens"`
			CacheWriteTokens int `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

func (r *responsesObject) stopReason(expected string) (string, error) {
	if expected != "" && expected != r.Status {
		return "", fmt.Errorf("openai responses: terminal event status %q disagrees with response status %q", expected, r.Status)
	}
	if r.Error != nil {
		e := r.Error.failure(r.Status)
		e.kind = r.ErrorType
		return "", e
	}
	switch r.Status {
	case "completed":
		return r.Status, nil
	case "incomplete":
		return cmp.Or(r.IncompleteDetails.Reason, r.Status), nil
	case "failed":
		return "", &responseError{status: r.Status, kind: r.ErrorType}
	case "cancelled":
		return "", &responseError{status: r.Status}
	case "queued", "in_progress":
		return "", fmt.Errorf("openai responses: non-terminal response status %q", r.Status)
	default:
		return "", fmt.Errorf("openai responses: unexpected response status %q", r.Status)
	}
}

type responsesPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type responsesItem struct {
	// raw is the protocol representation. The fields below it are a view
	// for validation and local projection, never a serialization schema.
	raw       json.RawMessage
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments *string         `json:"arguments"`
	Status    string          `json:"status"`
	Content   []responsesPart `json:"content"`
	Summary   []responsesPart `json:"summary"`
}

func decodeResponsesItem(raw json.RawMessage) (responsesItem, error) {
	// Other item kinds occupy output positions, but their payload schemas
	// (including the meaning of fields like "content") aren't ours.
	var tag struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(raw, &tag); err != nil {
		return responsesItem{}, err
	}
	i := responsesItem{raw: raw, Type: tag.Type, ID: tag.ID}
	switch tag.Type {
	case "function_call", "message", "reasoning":
		if err := json.Unmarshal(raw, &i); err != nil {
			return responsesItem{}, err
		}
	case "":
		return responsesItem{}, fmt.Errorf("missing output item type")
	}
	return i, nil
}

func decodeResponsesOutput(raw json.RawMessage) ([]responsesItem, error) {
	var array []json.RawMessage
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &array); err != nil {
			return nil, err
		}
	}
	items := make([]responsesItem, len(array))
	for n, raw := range array {
		i, err := decodeResponsesItem(raw)
		if err != nil {
			return nil, err
		}
		items[n] = i
	}
	return items, nil
}

// Use a JSON string in ProviderExtra rather than a decoded array or a
// RawMessage: both of the latter can become []any containing float64 after
// a persistence round trip. Only the array delimiters are constructed here.
func recordedResponsesOutput(items []responsesItem) string {
	var b strings.Builder
	b.WriteByte('[')
	for n, item := range items {
		if n != 0 {
			b.WriteByte(',')
		}
		b.Write(item.raw)
	}
	b.WriteByte(']')
	return b.String()
}

func (i *responsesItem) executable(responseStatus string) bool {
	return i.Type == "function_call" && (i.Status == "completed" || (i.Status == "" && responseStatus == "completed"))
}

// IDs can be bound late and observed repeatedly, but cannot move to another
// position. An absent item ID carries no evidence; a terminal call ID, in
// contrast, is required even on a non-executable function call.
func responsesClaim(indexes map[string]int, id string, index int, field string) error {
	if id != "" {
		if old, ok := indexes[id]; ok && old != index {
			return fmt.Errorf("duplicate %s %q", field, id)
		}
		indexes[id] = index
	}
	return nil
}

func validateResponsesOutput(items []responsesItem, status string) error {
	itemIDs := make(map[string]int)
	callIDs := make(map[string]int)
	for n := range items {
		i := &items[n]
		if err := responsesClaim(itemIDs, i.ID, n, "output item ID"); err != nil {
			return err
		}
		if i.Type != "function_call" {
			continue
		}
		if i.CallID == "" {
			return fmt.Errorf("function call item %q is missing call_id", i.ID)
		}
		if err := responsesClaim(callIDs, i.CallID, n, "call_id"); err != nil {
			return err
		}
		if i.executable(status) && i.Name == "" {
			return fmt.Errorf("function call %q is missing its name", i.CallID)
		}
	}
	return nil
}

func responsesHasText(parts []responsesPart, kind string) bool {
	for _, p := range parts {
		if p.Type == kind && p.Text != "" {
			return true
		}
	}
	return false
}

func (r *responsesObject) project(items []responsesItem, stop string) *llm.Response {
	out := &llm.Response{
		StopReason: stop,
		Usage: llm.Usage{
			PromptTokens:     r.Usage.InputTokens,
			CompletionTokens: r.Usage.OutputTokens,
			TotalTokens:      r.Usage.TotalTokens,
			CacheReadTokens:  r.Usage.InputTokensDetails.CachedTokens,
			CacheWriteTokens: r.Usage.InputTokensDetails.CacheWriteTokens,
		},
	}
	var text, thoughts, refusal strings.Builder
	for _, item := range items {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				switch part.Type {
				case "output_text":
					text.WriteString(part.Text)
				case "refusal":
					refusal.WriteString(part.Refusal)
				}
			}
		case "reasoning":
			parts, kind := item.Content, "reasoning_text"
			if !responsesHasText(parts, kind) {
				parts, kind = item.Summary, "summary_text"
			}
			for _, part := range parts {
				if part.Type == kind {
					thoughts.WriteString(part.Text)
				}
			}
		case "function_call":
			if item.executable(r.Status) {
				tc := llm.ToolCall{ID: item.CallID, Name: item.Name}
				if item.Arguments != nil {
					tc.Arguments = *item.Arguments
				}
				out.ToolCalls = append(out.ToolCalls, tc)
			}
		}
	}
	out.Content = text.String()
	out.Thoughts = thoughts.String()
	if refusal.Len() != 0 {
		out.Refusal = &llm.Refusal{Category: "refusal", Explanation: refusal.String()}
	}
	return out
}

func (a *responsesAttempt) finish(r *responsesObject, expected string, prefix *responsesPrefix) (*llm.Response, []llm.StreamEvent, error) {
	stop, err := r.stopReason(expected)
	if err != nil {
		return nil, nil, err
	}
	items, err := decodeResponsesOutput(r.Output)
	if err != nil {
		return nil, nil, fmt.Errorf("openai responses decode output item: %w", err)
	}
	feed := a.feed
	if feed != nil {
		items, err = feed.restoreReasoning(items)
		if err != nil {
			return nil, nil, err
		}
	}
	if err := validateResponsesOutput(items, r.Status); err != nil {
		return nil, nil, fmt.Errorf("openai responses: %w", err)
	}

	// Reconciliation is transactional at the reader boundary. It may
	// discover events, but none escape if a later slot or the seal fails.
	var events []llm.StreamEvent
	if feed != nil {
		events, err = feed.reconcile(items, r.Status)
		if err != nil {
			return nil, nil, err
		}
	}
	out := r.project(items, stop)
	receipt := responsesReceipt{output: recordedResponsesOutput(items)}
	if a.plan.store && (r.Store == nil || *r.Store) && strings.TrimSpace(r.ID) != "" {
		receipt.id = r.ID
	}
	if err := prefix.seal(receipt, out); err != nil {
		return nil, nil, err
	}
	return out, events, nil
}

// SSE framing is independent of the Responses event vocabulary. In
// particular, EOF is not a blank line and must not dispatch a partial frame.
type responsesSSE struct {
	lines *bufio.Scanner
	first bool
}

func newResponsesSSE(r io.Reader) responsesSSE {
	lines := bufio.NewScanner(r)
	lines.Buffer(make([]byte, 64<<10), responsesSSEMaxLine)

	// A full terminal response can occupy one very long line. Search each
	// byte once across short reads. CR terminates immediately; an optional
	// following LF belongs to that same delimiter, even across reads.
	scanned := 0
	skipLF := false
	lines.Split(func(b []byte, _ bool) (int, []byte, error) {
		start := 0
		if skipLF && len(b) != 0 {
			skipLF = false
			if b[0] == '\n' {
				start++
			}
		}
		if j := bytes.IndexAny(b[start+scanned:], "\r\n"); j >= 0 {
			i := start + scanned + j
			scanned = 0
			skipLF = b[i] == '\r'
			return i + 1, b[start:i], nil
		}
		scanned = len(b) - start
		return start, nil, nil
	})
	return responsesSSE{lines: lines, first: true}
}

func (s *responsesSSE) next() ([]byte, error) {
	var data []byte
	for s.lines.Scan() {
		line := s.lines.Bytes()
		if s.first {
			line = bytes.TrimPrefix(line, []byte("\uFEFF"))
			s.first = false
		}
		if len(line) == 0 {
			if len(data) != 0 {
				return data[:len(data)-1], nil
			}
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		if bytes.Equal(field, []byte("data")) {
			data = append(data, bytes.TrimPrefix(value, []byte(" "))...)
			data = append(data, '\n')
		}
	}
	if err := s.lines.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

type responsesStreamEvent struct {
	responsesErrorDetails
	Error       *responsesErrorDetails `json:"error"`
	ErrorType   string                 `json:"error_type"`
	Type        string                 `json:"type"`
	OutputIndex *int                   `json:"output_index"`
	ItemID      string                 `json:"item_id"`
	CallID      string                 `json:"call_id"`
	Name        string                 `json:"name"`
	Delta       json.RawMessage        `json:"delta"`
	Arguments   *string                `json:"arguments"`
	Item        json.RawMessage        `json:"item"`
	Response    json.RawMessage        `json:"response"`
}

func (e *responsesStreamEvent) index() (int, error) {
	if e.OutputIndex == nil || *e.OutputIndex < 0 {
		return 0, fmt.Errorf("%s is missing a valid output_index", e.Type)
	}
	return *e.OutputIndex, nil
}

func (e *responsesStreamEvent) deltaText() (string, error) {
	var s string
	if len(e.Delta) != 0 {
		if err := json.Unmarshal(e.Delta, &s); err != nil {
			return "", fmt.Errorf("decode delta: %w", err)
		}
	}
	return s, nil
}

// A feed belongs to one HTTP attempt. Only calls and finalized reasoning
// require incremental storage. Other output kinds still count when mapping
// the terminal array back to stream positions.
type responsesFeed struct {
	frames  responsesSSE
	slots   map[int]*responsesSlot
	itemIDs map[string]int
	callIDs map[string]int
}

type responsesSlot struct {
	kind      string
	id        string
	call      *responsesLiveCall
	reasoning *responsesItem
}

type responsesLiveCall struct {
	id      string
	name    string
	args    strings.Builder
	started bool // start produced in this attempt, not necessarily published
}

func newResponsesFeed(r io.Reader) *responsesFeed {
	return &responsesFeed{
		frames:  newResponsesSSE(r),
		slots:   make(map[int]*responsesSlot),
		itemIDs: make(map[string]int),
		callIDs: make(map[string]int),
	}
}

// Bind only information the observation actually supplies. The slot is the
// join key while item ID, canonical call ID and function name arrive.
func (f *responsesFeed) bind(index int, i *responsesItem) (*responsesSlot, error) {
	s := f.slots[index]
	if s == nil {
		s = &responsesSlot{kind: i.Type}
		if i.Type == "function_call" {
			s.call = new(responsesLiveCall)
		}
		f.slots[index] = s
	} else if s.kind != i.Type {
		return nil, fmt.Errorf("output index %d changed type from %q to %q", index, s.kind, i.Type)
	}
	if i.ID != "" {
		if s.id != "" && s.id != i.ID {
			return nil, fmt.Errorf("output index %d changed item ID from %q to %q", index, s.id, i.ID)
		}
		if err := responsesClaim(f.itemIDs, i.ID, index, "output item ID"); err != nil {
			return nil, err
		}
		s.id = i.ID
	}
	if c := s.call; c != nil {
		if i.CallID != "" {
			if c.id != "" && c.id != i.CallID {
				return nil, fmt.Errorf("item %q changed call_id from %q to %q", s.id, c.id, i.CallID)
			}
			if err := responsesClaim(f.callIDs, i.CallID, index, "call_id"); err != nil {
				return nil, err
			}
			c.id = i.CallID
		}
		if i.Name != "" {
			if c.name != "" && c.name != i.Name {
				return nil, fmt.Errorf("call %q changed function name from %q to %q", c.id, c.name, i.Name)
			}
			c.name = i.Name
		}
	}
	return s, nil
}

// A delta contributes bytes; a snapshot describes a prefix as a whole.
// Keep the bytes (not just a "saw a delta" flag) both to defer delivery
// until identification and to emit exactly the missing tail of a snapshot.
func (c *responsesLiveCall) advance(dst []llm.StreamEvent, delta string, snapshot *string, terminal bool) ([]llm.StreamEvent, error) {
	if snapshot != nil {
		known := c.args.String()
		switch {
		case strings.HasPrefix(*snapshot, known):
			delta = (*snapshot)[len(known):]
		case !terminal && strings.HasPrefix(known, *snapshot):
			// E.g. an added item with empty arguments after an early delta.
			delta = ""
		default:
			return nil, fmt.Errorf("inconsistent argument snapshot for call %q", c.id)
		}
	}
	c.args.WriteString(delta)
	if c.id == "" || c.name == "" {
		return dst, nil
	}
	if !c.started {
		c.started = true
		dst = append(dst, llm.StreamEvent{ToolCallStart: &llm.ToolCallStart{ID: c.id, Name: c.name}})
		delta = c.args.String()
	}
	if delta != "" {
		dst = append(dst, llm.StreamEvent{
			ToolCallDelta: &llm.ToolCallDelta{ID: c.id, ArgumentsDelta: delta},
		})
	}
	return dst, nil
}

func (f *responsesFeed) observe(e *responsesStreamEvent) ([]llm.StreamEvent, error) {
	var item responsesItem
	var delta string
	switch e.Type {
	case "error":
		d := &e.responsesErrorDetails
		if e.Error != nil {
			d = e.Error
		}
		err := d.failure("error")
		err.kind = e.ErrorType
		return nil, err

	case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		text, err := e.deltaText()
		if err != nil {
			return nil, err
		}
		if e.Type == "response.output_text.delta" {
			return []llm.StreamEvent{{DeltaText: text}}, nil
		}
		return []llm.StreamEvent{{DeltaThoughts: text}}, nil

	case "response.output_item.added", "response.output_item.done":
		var err error
		if item, err = decodeResponsesItem(e.Item); err != nil {
			return nil, fmt.Errorf("decode output item: %w", err)
		}
		if item.Type != "function_call" && (item.Type != "reasoning" || e.Type != "response.output_item.done") {
			return nil, nil
		}

	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		item = responsesItem{Type: "function_call", ID: e.ItemID, CallID: e.CallID, Name: e.Name}
		if e.Type == "response.function_call_arguments.done" {
			item.Arguments = e.Arguments
		} else {
			var err error
			delta, err = e.deltaText()
			if err != nil {
				return nil, err
			}
		}

	default:
		return nil, nil
	}

	index, err := e.index()
	if err != nil {
		return nil, err
	}
	slot, err := f.bind(index, &item)
	if err != nil {
		return nil, err
	}
	if item.Type == "reasoning" {
		if item.ID == "" {
			return nil, fmt.Errorf("invalid completed reasoning ID at output index %d", index)
		}
		slot.reasoning = &item
		return nil, nil
	}
	return slot.call.advance(nil, delta, item.Arguments, false)
}

// Only completed reasoning can be inserted. Never manufacture padding for
// a large index or reorder two items that are present in the terminal array.
func (f *responsesFeed) restoreReasoning(items []responsesItem) ([]responsesItem, error) {
	last := -1
	for n, slot := range f.slots {
		if slot.reasoning != nil {
			last = max(last, n)
		}
	}
	if last < 0 {
		return items, nil
	}

	out := make([]responsesItem, 0, len(items))
	for len(items) != 0 || len(out) <= last {
		n := len(out)
		if slot := f.slots[n]; slot != nil && slot.reasoning != nil {
			done := *slot.reasoning
			if len(items) != 0 && (items[0].ID == done.ID || (items[0].ID == "" && items[0].Type == "reasoning")) {
				t := &items[0]
				if t.Type != "reasoning" {
					return nil, fmt.Errorf("terminal reasoning item %q disagrees with output index %d", t.ID, n)
				}
				// Prefer finalized item text, but don't let a sparse done
				// item replace actual terminal reasoning with a summary.
				if !responsesHasText(done.Content, "reasoning_text") {
					done.Content = t.Content
				}
				if !responsesHasText(done.Summary, "summary_text") {
					done.Summary = t.Summary
				}
				// The display view may draw on both observations. For
				// replay, never construct a hybrid of signed, encrypted
				// or otherwise opaque fields from two different objects.
				done.raw = t.raw
				items = items[1:]
			}
			out = append(out, done)
			continue
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("missing output item before completed reasoning")
		}
		i := items[0]
		if pos, ok := f.itemIDs[i.ID]; ok && f.slots[pos].reasoning != nil {
			return nil, fmt.Errorf("terminal reasoning item %q disagrees with output index %d", i.ID, pos)
		}
		out = append(out, i)
		items = items[1:]
	}
	return out, nil
}

func (f *responsesFeed) reconcile(items []responsesItem, status string) ([]llm.StreamEvent, error) {
	var events []llm.StreamEvent
	for n := range items {
		i := &items[n]
		slot, err := f.bind(n, i)
		if err != nil {
			return nil, err
		}
		if !i.executable(status) {
			// Binding still checks the identity of an unfinished call, but
			// its presence in a snapshot isn't a reason to announce it.
			continue
		}
		c := slot.call
		if i.Arguments == nil && c.args.Len() != 0 {
			args := c.args.String()
			i.Arguments = &args
		}
		events, err = c.advance(events, "", i.Arguments, true)
		if err != nil {
			return nil, err
		}
	}
	if status == "completed" {
		for n, slot := range f.slots {
			if n >= len(items) && slot.call != nil {
				return nil, fmt.Errorf("openai responses: streamed function call is missing from terminal output")
			}
		}
	}
	return events, nil
}

func (f *responsesFeed) event() (*responsesStreamEvent, error) {
	data, err := f.frames.next()
	if err == io.EOF || bytes.Equal(data, []byte("[DONE]")) {
		return nil, fmt.Errorf("openai responses stream ended before terminal response event")
	}
	if err != nil {
		return nil, fmt.Errorf("openai responses stream read: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}

	var e responsesStreamEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("openai responses: decode SSE event: %w", err)
	}
	return &e, nil
}

// Decoder state never survives replacement. The plan remains evidence of
// what was actually requested even after publication takes away replay.
type responsesAttempt struct {
	plan   responsesPlan
	body   io.ReadCloser
	feed   *responsesFeed
	replay *responsesRequest
}

func (a *responsesAttempt) next(prefix *responsesPrefix) (*llm.Response, []llm.StreamEvent, error) {
	if a.feed == nil {
		var r responsesObject
		if err := json.NewDecoder(a.body).Decode(&r); err != nil {
			return nil, nil, fmt.Errorf("openai responses decode response: %w", err)
		}
		return a.finish(&r, "", prefix)
	}

	e, err := a.feed.event()
	if e == nil || err != nil {
		return nil, nil, err
	}
	var out *llm.Response
	var events []llm.StreamEvent
	switch e.Type {
	case "response.completed", "response.incomplete", "response.failed", "response.cancelled":
		status := strings.TrimPrefix(e.Type, "response.")
		var r responsesObject
		switch {
		case len(e.Response) == 0:
			err = &responseError{status: status}
		default:
			if err = json.Unmarshal(e.Response, &r); err != nil {
				err = fmt.Errorf("decode response: %w", err)
			} else {
				out, events, err = a.finish(&r, status, prefix)
			}
		}
	default:
		events, err = a.feed.observe(e)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("openai responses stream %s: %w", e.Type, err)
	}
	return out, events, nil
}

// An exchange owns the logical prefix and publishes at most one terminal
// result. Only its current attempt owns an HTTP body and a stream decoder.
type responsesExchange struct {
	provider  *responsesProvider
	streaming bool
	prefix    responsesPrefix
	ctx       context.Context
	cancel    context.CancelFunc

	// Only Close races the reader. It can detach and close the active body
	// or cancel a POST that hasn't installed a body yet. It never mutates
	// decoder state being used by the reader.
	mu     sync.Mutex
	active *responsesAttempt

	pending []llm.StreamEvent
	cur     llm.StreamEvent
	final   *llm.Response
	err     error
}

func (p *responsesProvider) begin(ctx context.Context, req llm.Request, streaming bool) (*responsesExchange, error) {
	options, err := p.requestOptions(req, streaming)
	if err != nil {
		return nil, err
	}
	// Use the effective wire model. A URL alone identifies neither the
	// credential namespace nor the input encoding policy. Persist only a
	// fingerprint, never the possibly credential-bearing URL or API key.
	identity := []any{p.url, options["model"], p.apiKey, p.hosted, p.webp}
	if options["provider"] != nil || options["models"] != nil {
		// Routing overrides can select a different backend at the same
		// address. Preserve the legacy scope for unqualified requests.
		identity = append(identity, options["provider"], options["models"])
	}
	scope, err := json.Marshal(identity)
	if err != nil {
		return nil, fmt.Errorf("openai responses: identify endpoint: %w", err)
	}
	x := &responsesExchange{
		provider:  p,
		streaming: streaming,
		prefix: responsesPrefix{
			endpoint: fmt.Sprintf("%x", sha256.Sum256(scope)),
			digest:   sha256.New(),
		},
	}
	d := &responsesRequest{options: options, messages: make([]llm.Message, len(req.Messages))}
	plan := responsesPlan{store: !p.replayOnly.Load()}
	for n, m := range req.Messages {
		d.messages[n] = retainedResponsesMessage(m)
		if err := x.prefix.include(d.messages[n]); err != nil {
			return nil, err
		}
		if m.Role != llm.RoleAssistant {
			continue
		}
		r := x.prefix.read(m)
		if r.output != "" {
			d.recorded = append(d.recorded, responsesSubstitution{at: n, output: r.output})
		}
		if plan.store && r.id != "" {
			if _, missing := p.missingIDs.Load(r.id); !missing {
				plan.previousID, plan.from = r.id, n+1
			}
		}
	}

	x.ctx, x.cancel = context.WithCancel(ctx)
	if err := x.start(d, plan, nil); err != nil {
		x.Close()
		return nil, err
	}
	return x, nil
}

// open performs exactly one handshake. Stream reports handshake failures
// synchronously; consumption of a successful response body remains lazy.
func (x *responsesExchange) open(d *responsesRequest, plan responsesPlan) error {
	resp, err := d.post(x.ctx, x.provider, plan)
	if err != nil {
		return err
	}
	a := &responsesAttempt{body: resp.Body, plan: plan}
	if plan.store {
		a.replay = d
	}
	if x.streaming {
		ct := resp.Header.Get("Content-Type")
		media, _, _ := strings.Cut(ct, ";")
		// Permit an absent type and the application/event-stream spelling
		// used by compatible servers, but don't feed a JSON body to SSE.
		if ct != "" && !strings.EqualFold(strings.TrimSpace(media), "text/event-stream") &&
			!strings.EqualFold(strings.TrimSpace(media), "application/event-stream") {
			defer resp.Body.Close()
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
			return fmt.Errorf("openai responses stream: expected SSE, got %s: %s", ct, strings.TrimSpace(string(snippet)))
		}
		a.feed = newResponsesFeed(resp.Body)
	}
	x.mu.Lock()
	err = x.ctx.Err()
	if err == nil {
		x.active = a
	}
	x.mu.Unlock()
	if err != nil {
		_ = a.body.Close()
	}
	return err
}

// The same transition rule handles handshake and generation failures.
// Chained -> fresh stored -> explicit is the longest possible path. A
// missing ID never causes a search for another ID during this operation.
func (x *responsesExchange) start(d *responsesRequest, plan responsesPlan, failure error) error {
	for {
		if failure != nil {
			var e *responseError
			if !errors.As(failure, &e) {
				return failure
			}
			switch plan.rejection(e) {
			case responsesMissingContinuation:
				x.provider.missingIDs.Store(plan.previousID, struct{}{})
				plan = responsesPlan{store: true}
			case responsesUnsupportedState:
				x.provider.replayOnly.Store(true)
				plan = responsesPlan{}
			case responsesOtherFailure:
				return failure
			}
			// Learn even from the failure of a published attempt. Knowing
			// what to do next turn isn't permission to replace this one.
			if d == nil || x.ctx.Err() != nil {
				return failure
			}
		}
		if x.provider.replayOnly.Load() {
			plan = responsesPlan{}
		}
		failure = x.open(d, plan)
		if failure == nil {
			return nil
		}
	}
}

func (x *responsesExchange) detach() {
	x.mu.Lock()
	a := x.active
	x.active = nil
	x.mu.Unlock()
	if a != nil {
		_ = a.body.Close()
	}
}

func (x *responsesExchange) Close() {
	x.cancel()
	x.detach()
}

// Read one REST object or one SSE frame. Only Next requests publication;
// Call may discard arbitrarily many provisional events and still replace
// an explicitly rejected continuation.
func (x *responsesExchange) step(publish bool) {
	x.mu.Lock()
	a := x.active
	x.mu.Unlock()
	if err := x.ctx.Err(); err != nil {
		x.detach()
		x.err = err
		return
	}

	out, events, err := a.next(&x.prefix)
	if err != nil {
		x.detach()
		x.err = x.start(a.replay, a.plan, err)
		if x.err != nil {
			x.cancel()
		}
		return
	}
	if publish && len(events) != 0 {
		// Next will return an event from this batch without another read.
		a.replay = nil
		x.pending = events
	}
	if out != nil {
		x.final = out
		x.Close()
	}
}

func (p *responsesProvider) Call(ctx context.Context, req llm.Request) (*llm.Response, error) {
	x, err := p.begin(ctx, req, p.callStream)
	if err != nil {
		return nil, err
	}
	defer x.Close()
	for x.final == nil && x.err == nil {
		x.step(false)
	}
	return x.final, x.err
}

func (p *responsesProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	x, err := p.begin(ctx, req, true)
	if err != nil {
		return nil, err
	}
	return x, nil
}

func (x *responsesExchange) Next() bool {
	x.cur = llm.StreamEvent{}
	for len(x.pending) == 0 && x.final == nil && x.err == nil {
		x.step(true)
	}
	if len(x.pending) == 0 {
		return false
	}
	x.cur = x.pending[0]
	x.pending[0] = llm.StreamEvent{}
	x.pending = x.pending[1:]
	if len(x.pending) == 0 {
		x.pending = nil
	}
	return true
}

func (x *responsesExchange) Event() llm.StreamEvent  { return x.cur }
func (x *responsesExchange) Response() *llm.Response { return x.final }
func (x *responsesExchange) Err() error              { return x.err }

var _ llm.Stream = (*responsesExchange)(nil)
