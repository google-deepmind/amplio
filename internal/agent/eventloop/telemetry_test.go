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

package eventloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"amplio/internal/db"
	"amplio/internal/eventstream"
	"amplio/internal/llm"
	"amplio/internal/tool"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func traceRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = tp.Shutdown(context.Background())
	})
	return recorder
}

func spanAttrs(span sdktrace.ReadOnlySpan) map[string]attribute.Value {
	attrs := make(map[string]attribute.Value)
	for _, a := range span.Attributes() {
		attrs[string(a.Key)] = a.Value
	}
	return attrs
}

func TestAgentStepTraces(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", interactive), func(t *testing.T) {
			recorder := traceRecorder(t)
			store, runID, registry := testSetup(t)
			provider := &llm.MockProvider{Model: "test-model", Responses: []llm.Response{
				{Content: "private reasoning", ToolCalls: []llm.ToolCall{
					{ID: "call-ok", Name: "echo", Arguments: `{"secret":"private argument"}`},
					{ID: "call-bad", Name: "missing", Arguments: `{}`},
				}, Usage: llm.Usage{PromptTokens: 42, CompletionTokens: 7, CacheReadTokens: 4}, StopReason: "tool_calls"},
				{Content: "private final answer", StopReason: "stop"},
			}}
			ag := newT(testCfg{
				RunID: runID, Store: store, Registry: registry, LLM: provider,
				SessionID: "worker", AgentType: "standard_agent", Task: "private task",
				Interactive: interactive, IdleTimeout: 50 * time.Millisecond,
				Broadcaster: eventstream.NoOpBroadcaster{},
				Tools: []*tool.Tool{{Name: "echo", ParamType: &struct{}{}, Execute: func(context.Context, json.RawMessage) (*tool.Result, error) {
					return &tool.Result{Content: "private output"}, nil
				}}},
			})
			if err := ag.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			spans := recorder.Ended()
			if len(spans) != 6 {
				t.Fatalf("ended spans=%d, want 2 steps + 2 calls + 2 tools", len(spans))
			}
			roots := map[int]sdktrace.ReadOnlySpan{}
			for _, span := range spans {
				attrs := spanAttrs(span)
				if attrs["amplio.run.id"].AsString() != runID || attrs["amplio.session.id"].AsString() != "worker" {
					t.Errorf("missing correlation on %s", span.Name())
				}
				if strings.Contains(fmt.Sprint(span.Attributes(), span.Events(), span.Status()), "private") {
					t.Errorf("content leaked in %s", span.Name())
				}
				if span.Name() == "amplio.step" {
					if span.Parent().IsValid() {
						t.Error("step must start its own trace")
					}
					roots[int(attrs["amplio.step"].AsInt64())] = span
				}
				if span.Name() == "chat test-model" && attrs["amplio.step"].AsInt64() == 1 {
					if attrs["gen_ai.usage.input_tokens"].AsInt64() != 42 || attrs["gen_ai.usage.output_tokens"].AsInt64() != 7 {
						t.Error("generation usage was not recorded")
					}
				}
				if span.Name() == "execute_tool missing" && span.Status().Code != codes.Error {
					t.Error("missing tool should have error status")
				}
			}
			if len(roots) != 2 || roots[1].SpanContext().TraceID() == roots[2].SpanContext().TraceID() {
				t.Fatal("expected independent step traces")
			}
			for _, span := range spans {
				if span.Name() == "amplio.step" {
					continue
				}
				root := roots[int(spanAttrs(span)["amplio.step"].AsInt64())]
				if span.Parent().SpanID() != root.SpanContext().SpanID() || span.SpanContext().TraceID() != root.SpanContext().TraceID() {
					t.Errorf("wrong step parent on %s", span.Name())
				}
			}
			if interactive && time.Since(roots[2].EndTime()) < 40*time.Millisecond {
				t.Error("step span included idle waiting")
			}
			session, err := store.GetSession(context.Background(), runID, "worker")
			if err != nil {
				t.Fatal(err)
			}
			want := db.SessionConcluded
			if interactive {
				want = db.SessionIdle
			}
			if session.Status != want {
				t.Errorf("status=%s, want %s", session.Status, want)
			}
		})
	}
}

type failingTraceProvider struct{ llm.MockProvider }

func (p *failingTraceProvider) Call(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("private provider response")
}

func TestFailedGenerationTrace(t *testing.T) {
	recorder := traceRecorder(t)
	store, runID, registry := testSetup(t)
	ag := newT(testCfg{RunID: runID, Store: store, Registry: registry,
		LLM: &failingTraceProvider{}, SessionID: "worker", Task: "private task"})
	if err := ag.Run(context.Background()); err == nil {
		t.Fatal("expected model failure")
	}
	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("spans=%d, want step and failed call", len(spans))
	}
	for _, span := range spans {
		if span.Status().Code != codes.Error {
			t.Errorf("%s missing error status", span.Name())
		}
		if strings.Contains(fmt.Sprint(span.Attributes(), span.Events(), span.Status()), "private") {
			t.Error("provider error content was exported")
		}
	}
}
