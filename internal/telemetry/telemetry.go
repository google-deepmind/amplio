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

// Package telemetry exports optional agent traces using OTLP/HTTP.
package telemetry

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Init is called once at process startup. No exporter or worker is created
// unless OTEL_TRACES_EXPORTER=otlp. Endpoint, headers, TLS, sampling and batch
// settings use the standard OTel environment variables.
func Init(ctx context.Context) (func() error, error) {
	noop := func() error { return nil }
	if disabled, _ := strconv.ParseBool(os.Getenv("OTEL_SDK_DISABLED")); disabled {
		return noop, nil
	}
	switch os.Getenv("OTEL_TRACES_EXPORTER") {
	case "", "none":
		return noop, nil
	case "otlp":
	default:
		return nil, fmt.Errorf("OTEL_TRACES_EXPORTER must be otlp or none")
	}
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if protocol != "" && protocol != "http/protobuf" {
		return nil, fmt.Errorf("OTLP traces require protocol http/protobuf")
	}
	r, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "amplio")),
		resource.WithFromEnv(), resource.WithTelemetrySDK())
	if err != nil {
		return nil, fmt.Errorf("trace resource: %w", err)
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(r), sdktrace.WithBatcher(exporter))
	otel.SetTracerProvider(tp)
	return func() error {
		// The run context is usually cancelled by shutdown. Use a fresh, bounded
		// context so both serve and short-lived headless runs can drain the batch.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return tp.Shutdown(ctx)
	}, nil
}

type stepKey struct{}

// StartStep starts a new trace for each execution attempt of a durable step.
// Correlation survives resume through IDs, without keeping spans open across
// process restarts. Metadata stays local to the context; it is not baggage.
func StartStep(ctx context.Context, runID, sessionID, parentID, agentType string, step int) (context.Context, trace.Span) {
	attrs := []attribute.KeyValue{
		attribute.String("amplio.run.id", runID),
		attribute.String("amplio.session.id", sessionID),
		attribute.Int("amplio.step", step),
		attribute.String("gen_ai.agent.name", agentType),
	}
	if parentID != "" {
		attrs = append(attrs, attribute.String("amplio.parent_session.id", parentID))
	}
	ctx, span := otel.Tracer("amplio").Start(ctx, "amplio.step", trace.WithNewRoot(), trace.WithAttributes(attrs...))
	if span.IsRecording() {
		ctx = context.WithValue(ctx, stepKey{}, attrs)
	}
	return ctx, span
}

// Start records a child operation only inside an instrumented agent step.
// This keeps shared tools used by uninstrumented system workers from emitting
// unrelated root spans. Each child carries the step's correlation attributes.
func Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	attrs, ok := ctx.Value(stepKey{}).([]attribute.KeyValue)
	if !ok {
		return ctx, trace.SpanFromContext(context.Background())
	}
	opts = append(opts, trace.WithAttributes(attrs...))
	return otel.Tracer("amplio").Start(ctx, name, opts...)
}

// Fail records a category, never arbitrary error text that could contain
// prompts, tool output, request URLs or credentials.
func Fail(span trace.Span, kind string) {
	span.SetStatus(codes.Error, kind)
	span.SetAttributes(attribute.String("error.type", kind))
}
