package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"tracing-demo/internal/tracing"
)

var fooGVR = schema.GroupVersionResource{
	Group:    "example.com",
	Version:  "v1",
	Resource: "foos",
}

func k8sConfig() (*rest.Config, error) {
	// In-cluster first, fall back to local kubeconfig for testing outside the cluster.
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func main() {
	ctx := context.Background()

	shutdown, err := tracing.Init(ctx, "crd-creator")
	if err != nil {
		log.Fatalf("tracing init: %v", err)
	}
	defer shutdown(ctx)

	tracer := otel.Tracer("crd-creator")
	ctx, span := tracer.Start(ctx, "create-foo")
	defer span.End()

	log.Printf("trace id: %s", span.SpanContext().TraceID())

	cfg, err := k8sConfig()
	if err != nil {
		log.Fatalf("k8s config: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("dynamic client: %v", err)
	}

	namespace := envOr("NAMESPACE", "default")
	name := fmt.Sprintf("foo-sample-%d", time.Now().Unix())

	// This is the key step: stash the current trace context as
	// annotations on the object we're about to create, so the
	// controller can pick it back up later and link its span as a
	// child of this one, even though there is no direct RPC call
	// between the two processes.
	annotations := tracing.InjectToMap(ctx)

	foo := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "example.com/v1",
			"kind":       "Foo",
			"metadata": map[string]interface{}{
				"name":        name,
				"namespace":   namespace,
				"annotations": toStringMapIface(annotations),
			},
			"spec": map[string]interface{}{
				"message": "hello from creator",
			},
		},
	}

	created, err := dyn.Resource(fooGVR).Namespace(namespace).Create(ctx, foo, metav1.CreateOptions{})
	if err != nil {
		span.RecordError(err)
		log.Fatalf("create foo: %v", err)
	}

	log.Printf("created Foo %s/%s (uid=%s)", namespace, created.GetName(), created.GetUID())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func toStringMapIface(m map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
