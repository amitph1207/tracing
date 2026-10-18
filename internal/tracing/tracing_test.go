package tracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestPropagationAndPolicies(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := trace.NewTracerProvider(trace.WithSpanProcessor(recorder), trace.WithSampler(trace.ParentBased(trace.AlwaysSample())))
	defer provider.Shutdown(context.Background())
	tracer := provider.Tracer("test")
	ctx, root := tracer.Start(context.Background(), "root")
	state, err := oteltrace.ParseTraceState("vendor=value")
	if err != nil {
		t.Fatal(err)
	}
	ctx = oteltrace.ContextWithSpanContext(ctx, root.SpanContext().WithTraceState(state))
	annotations := InjectAnnotations(ctx, map[string]string{"unrelated": "keep", AnnotationPrefix + "tracestate": "old=state"})
	if annotations["unrelated"] != "keep" || annotations[AnnotationPrefix+"tracestate"] != "vendor=value" {
		t.Fatal(annotations)
	}
	remote := oteltrace.SpanContextFromContext(ExtractAnnotations(context.Background(), annotations))
	if !remote.IsRemote() || remote.TraceID() != root.SpanContext().TraceID() || remote.SpanID() != root.SpanContext().SpanID() {
		t.Fatal(remote)
	}
	root.End() // Child spans can be started after a parent has ended.
	_, child := StartAttempt(context.Background(), tracer, "child", "child", InjectToMap(ctx))
	if child.SpanContext().TraceID() != remote.TraceID() || child.SpanContext().SpanID() == remote.SpanID() {
		t.Fatal("child identity")
	}
	child.End()
	_, linked := StartAttempt(context.Background(), tracer, "linked", "linked", InjectToMap(ctx))
	if linked.SpanContext().TraceID() == remote.TraceID() {
		t.Fatal("linked span reused trace")
	}
	linked.End()
	spans := recorder.Ended()
	if spans[1].Parent().SpanID() != remote.SpanID() || spans[2].Parent().IsValid() || len(spans[2].Links()) != 1 || spans[2].Links()[0].SpanContext.SpanID() != remote.SpanID() {
		t.Fatal("parent/link relationship")
	}
	clean := InjectAnnotations(oteltrace.ContextWithSpanContext(context.Background(), root.SpanContext()), annotations)
	if _, ok := clean[AnnotationPrefix+"tracestate"]; ok {
		t.Fatal("stale tracestate survived")
	}
}

func TestInvalidAndUnsampled(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := trace.NewTracerProvider(trace.WithSpanProcessor(recorder), trace.WithSampler(trace.ParentBased(trace.AlwaysSample())))
	defer provider.Shutdown(context.Background())
	tracer := provider.Tracer("test")
	for _, carrier := range []map[string]string{nil, {"traceparent": "broken", "tracestate": "vendor=value"}} {
		_, span := StartAttempt(context.Background(), tracer, "fallback", "child", carrier)
		if !span.SpanContext().IsValid() || !span.IsRecording() {
			t.Fatal("missing/invalid context should create sampled root")
		}
		span.End()
	}
	_, root := tracer.Start(context.Background(), "unsampled-source")
	remote := root.SpanContext().WithTraceFlags(0)
	carrier := InjectToMap(oteltrace.ContextWithSpanContext(context.Background(), remote))
	_, child := StartAttempt(context.Background(), tracer, "dropped", "child", carrier)
	if child.IsRecording() || child.SpanContext().IsSampled() || child.SpanContext().TraceID() != remote.TraceID() {
		t.Fatal("parent-based sampling ignored")
	}
	child.End()
	root.End()
	if len(recorder.Ended()) != 3 {
		t.Fatalf("exported %d spans", len(recorder.Ended()))
	}
}
