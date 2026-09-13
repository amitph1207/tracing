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
	otel.GetTextMapPropagator().Inject(ctx, carrier)
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
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}
