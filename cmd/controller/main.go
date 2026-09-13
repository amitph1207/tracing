package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"tracing-demo/internal/tracing"
)

var fooGVR = schema.GroupVersionResource{
	Group:    "example.com",
	Version:  "v1",
	Resource: "foos",
}

func k8sConfig() (*rest.Config, error) {
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

	shutdown, err := tracing.Init(ctx, "foo-controller")
	if err != nil {
		log.Fatalf("tracing init: %v", err)
	}
	defer shutdown(ctx)

	tracer := otel.Tracer("foo-controller")

	cfg, err := k8sConfig()
	if err != nil {
		log.Fatalf("k8s config: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("dynamic client: %v", err)
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, 30*time.Second)
	informer := factory.ForResource(fooGVR).Informer()

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			reconcileFoo(ctx, tracer, u)
		},
	})

	log.Println("controller started, watching Foo objects...")
	stop := make(chan struct{})
	defer close(stop)
	informer.Run(stop)
}

// reconcileFoo is the "business logic": pull the trace context that the
// creator stashed in the object's annotations, start a child span linked
// to it, do the (trivial) work, and record success/failure on the span.
func reconcileFoo(parentCtx context.Context, tracer oteltrace.Tracer, u *unstructured.Unstructured) {
	annotations := u.GetAnnotations()

	// Rebuild a context carrying the remote (creator-side) trace,
	// so the span we start below becomes its child.
	ctx := tracing.ExtractFromMap(parentCtx, annotations)

	ctx, span := tracer.Start(ctx, "reconcile-foo",
		oteltrace.WithAttributes(
			attribute.String("k8s.namespace", u.GetNamespace()),
			attribute.String("k8s.name", u.GetName()),
			attribute.String("k8s.uid", string(u.GetUID())),
		),
	)
	defer span.End()

	log.Printf("reconciling %s/%s (trace_id=%s)", u.GetNamespace(), u.GetName(), span.SpanContext().TraceID())

	if err := writeToTmp(ctx, tracer, u); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		log.Printf("reconcile error: %v", err)
		return
	}

	span.SetStatus(codes.Ok, "reconciled")
}

// writeToTmp is deliberately its own child span, so you can see in Tempo
// that "reconcile-foo" has a sub-span for the actual work being done.
func writeToTmp(ctx context.Context, tracer oteltrace.Tracer, u *unstructured.Unstructured) error {
	_, span := tracer.Start(ctx, "write-to-disk")
	defer span.End()

	data, err := json.MarshalIndent(u.Object, "", "  ")
	if err != nil {
		return err
	}

	path := fmt.Sprintf("/tmp/foo-%s-%s.json", u.GetNamespace(), u.GetName())
	span.SetAttributes(attribute.String("file.path", path))

	if err := os.WriteFile(path, data, 0644); err != nil {
		return err
	}

	log.Printf("wrote %s", path)
	return nil
}
