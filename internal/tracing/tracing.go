package tracing

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// Init wires up a TracerProvider that exports spans via OTLP/gRPC.
// Endpoint defaults to localhost:4317, which is where the injected
// sidecar collector listens.
func Init(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:4317"
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp.Shutdown, nil
}

// InjectToMap puts the current trace context from ctx into a plain
// map[string]string so it can be stashed as k8s annotations.
func InjectToMap(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	out := map[string]string{}
	for k, v := range carrier {
		out[k] = v
	}
	return out
}

// ExtractFromMap rebuilds a context carrying the remote trace context
// found in the given map (e.g. object annotations).
func ExtractFromMap(ctx context.Context, m map[string]string) context.Context {
	carrier := propagation.MapCarrier{}
	for k, v := range m {
		carrier[k] = v
	}
	return propagation.TraceContext{}.Extract(ctx, carrier)
}

const AnnotationPrefix = "tracing.example.com/"

// InjectAnnotations preserves unrelated annotations and removes stale tracestate.
func InjectAnnotations(ctx context.Context, annotations map[string]string) map[string]string {
	out := make(map[string]string, len(annotations)+2)
	for k, v := range annotations {
		out[k] = v
	}
	delete(out, AnnotationPrefix+"traceparent")
	delete(out, AnnotationPrefix+"tracestate")
	for k, v := range InjectToMap(ctx) {
		out[AnnotationPrefix+k] = v
	}
	return out
}

func ExtractAnnotations(ctx context.Context, annotations map[string]string) context.Context {
	return ExtractFromMap(ctx, map[string]string{
		"traceparent": annotations[AnnotationPrefix+"traceparent"],
		"tracestate":  annotations[AnnotationPrefix+"tracestate"],
	})
}

// StartAttempt supports an explicit single-trace policy and an async link policy.
// WithNewRoot preserves cancellation on ctx while choosing a new trace identity.
func StartAttempt(ctx context.Context, tracer trace.Tracer, name, mode string, carrier map[string]string, attrs ...trace.SpanStartOption) (context.Context, trace.Span) {
	remote := trace.SpanContextFromContext(ExtractFromMap(context.Background(), carrier))
	if mode == "linked" {
		attrs = append(attrs, trace.WithNewRoot())
		if remote.IsValid() {
			attrs = append(attrs, trace.WithLinks(trace.Link{SpanContext: remote}))
		}
	} else {
		ctx = trace.ContextWithRemoteSpanContext(ctx, remote)
	}
	return tracer.Start(ctx, name, attrs...)
}
