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

package tool

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"amplio/internal/llm"
	"amplio/internal/telemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestParallelToolTraces(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = tp.Shutdown(context.Background())
	})
	ctx, root := telemetry.StartStep(context.Background(), "run", "agent", "", "standard_agent", 1)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	tools := []*Tool{}
	for _, name := range []string{"ok", "fail"} {
		tools = append(tools, &Tool{Name: name, Execute: func(context.Context, json.RawMessage) (*Result, error) {
			started <- struct{}{}
			<-release
			return &Result{IsError: name == "fail", Content: "sensitive tool result"}, nil
		}})
	}
	done := make(chan []CallResult, 1)
	go func() {
		done <- ExecuteAll(ctx, []llm.ToolCall{{ID: "a", Name: "ok"}, {ID: "b", Name: "fail"}}, ByName(tools), nil)
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("instrumentation serialized tool execution")
		}
	}
	close(release)
	results := <-done
	root.End()
	if results[0].Result.IsError || !results[1].Result.IsError {
		t.Fatal("tool results changed")
	}
	spans := recorder.Ended()
	if len(spans) != 3 {
		t.Fatalf("spans=%d, want step and 2 tools", len(spans))
	}
	for _, span := range spans {
		if span.Name() == "amplio.step" {
			continue
		}
		if span.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Error("parallel tool lost its parent span")
		}
		if (span.Status().Code == codes.Error) != (span.Name() == "execute_tool fail") {
			t.Errorf("wrong status for %s: %v", span.Name(), span.Status())
		}
	}
}
