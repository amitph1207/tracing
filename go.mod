module tracing-demo

go 1.22

require (
	go.opentelemetry.io/otel v1.24.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.24.0
	go.opentelemetry.io/otel/sdk v1.24.0
	go.opentelemetry.io/otel/trace v1.24.0
	k8s.io/apimachinery v0.29.3
	k8s.io/client-go v0.29.3
)
