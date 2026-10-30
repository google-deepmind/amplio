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

package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func cleanEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "OTEL_") {
			t.Setenv(name, "")
		}
	}
	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
}

func TestOTLPHTTPFlushOnShutdown(t *testing.T) {
	cleanEnv(t)
	received := make(chan *collectortrace.ExportTraceServiceRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/traces" || r.Method != http.MethodPost {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/x-protobuf" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing protobuf content type or configured authentication header")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request collectortrace.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- &request
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1/unused")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", server.URL+"/custom/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "Authorization=Bearer%20test-token")
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "60000")
	t.Setenv("OTEL_SERVICE_NAME", "amplio-test")
	ctx, cancel := context.WithCancel(context.Background())
	shutdown, err := Init(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown() })
	ctx, root := StartStep(ctx, "run-1", "agent-1", "parent-1", "standard_agent", 2)
	_, child := Start(ctx, "execute_tool test")
	child.SetAttributes(attribute.String("gen_ai.tool.name", "test"))
	child.End()
	root.End()
	cancel()
	if err := shutdown(); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-received:
		if len(request.ResourceSpans) != 1 {
			t.Fatalf("resource spans: %d", len(request.ResourceSpans))
		}
		resource := request.ResourceSpans[0]
		foundService := false
		for _, attr := range resource.Resource.Attributes {
			if attr.Key == "service.name" && attr.Value.GetStringValue() == "amplio-test" {
				foundService = true
			}
		}
		if !foundService {
			t.Error("OTEL_SERVICE_NAME was not applied")
		}
		count := 0
		for _, scope := range resource.ScopeSpans {
			count += len(scope.Spans)
			for _, span := range scope.Spans {
				if span.EndTimeUnixNano < span.StartTimeUnixNano || span.StartTimeUnixNano == 0 {
					t.Error("missing or invalid span timestamps")
				}
			}
		}
		if count != 2 {
			t.Errorf("span count: %d, want 2", count)
		}
	default:
		t.Fatal("shutdown did not flush the pending batch")
	}
}

func TestInitOptInAndProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, exporter, disabled, protocol string
		wantError                          bool
	}{
		{name: "default"},
		{name: "none", exporter: "none"},
		{name: "sdk disabled", exporter: "otlp", disabled: "true", protocol: "grpc"},
		{name: "unknown exporter", exporter: "unsupported", wantError: true},
		{name: "grpc", exporter: "otlp", protocol: "grpc", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("OTEL_TRACES_EXPORTER", tc.exporter)
			t.Setenv("OTEL_SDK_DISABLED", tc.disabled)
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", tc.protocol)
			previous := otel.GetTracerProvider()
			shutdown, err := Init(context.Background())
			if (err != nil) != tc.wantError {
				t.Fatalf("Init error: %v", err)
			}
			if err == nil {
				if err := shutdown(); err != nil {
					t.Fatal(err)
				}
			}
			if otel.GetTracerProvider() != previous {
				t.Error("disabled or rejected configuration changed the tracer provider")
			}
		})
	}
}

func TestCollectorFailureIsAsynchronous(t *testing.T) {
	cleanEnv(t)
	attempted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case attempted <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "50")
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "60000")
	shutdown, err := Init(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown() })
	_, span := StartStep(context.Background(), "run", "agent", "", "standard_agent", 1)
	span.End()
	// Export errors surface during the bounded shutdown, not in the agent's
	// execution path. A missing collector does not prevent startup or End.
	if err := shutdown(); err == nil {
		t.Fatal("expected a failed export during shutdown")
	}
	select {
	case <-attempted:
	default:
		t.Fatal("exporter did not contact the collector")
	}
}
