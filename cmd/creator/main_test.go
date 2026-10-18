package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	"tracing-demo/internal/demo"
	"tracing-demo/internal/tracing"
)

func TestRolloutBoundaries(t *testing.T) {
	for _, scenario := range []string{"ready", "timeout", "superseded"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			old := otel.GetTracerProvider()
			otel.SetTracerProvider(provider)
			defer func() { otel.SetTracerProvider(old); provider.Shutdown(ctx) }()
			client := fake.NewSimpleDynamicClient(runtime.NewScheme())
			var stored *unstructured.Unstructured
			client.PrependReactor("create", "foos", func(action ktesting.Action) (bool, runtime.Object, error) {
				stored = action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
				stored.SetNamespace(action.GetNamespace())
				stored.SetGeneration(1)
				stored.SetUID("test-uid")
				return true, stored.DeepCopy(), nil
			})
			client.PrependReactor("get", "foos", func(action ktesting.Action) (bool, runtime.Object, error) {
				current := stored.DeepCopy()
				if scenario == "ready" {
					_ = unstructured.SetNestedField(current.Object, int64(1), "status", "observedGeneration")
					_ = unstructured.SetNestedField(current.Object, "bar", "status", "childName")
					addReadyChildren(t, client, current)
				}
				if scenario == "superseded" {
					current.SetGeneration(2)
				}
				return true, current, nil
			})
			obj := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "example.com/v1", "kind": "Foo"}}
			obj.SetName("test")
			ctx, rollout := provider.Tracer("test").Start(ctx, "foo.rollout")
			err := submitAndWait(ctx, client, client.Resource(demo.FooGVR).Namespace("default"), obj, map[string]interface{}{"message": "hello"}, "valid", "create", 5*time.Millisecond, rollout)
			if (scenario == "ready") != (err == nil) {
				t.Fatalf("scenario=%s error=%v", scenario, err)
			}
			if scenario == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error=%v", err)
			}
			ended := recorder.Ended()
			if len(ended) != 1 || ended[0].Name() != "create-foo" {
				t.Fatal("submission should end before rollout")
			}
			if ended[0].Parent().SpanID() != rollout.SpanContext().SpanID() {
				t.Fatal("submission parent")
			}
			incoming := tracing.ExtractAnnotations(context.Background(), stored.GetAnnotations())
			if trace.SpanContextFromContext(incoming).SpanID() != ended[0].SpanContext().SpanID() {
				t.Fatal("handoff does not reference the submitting span")
			}
			// The handoff references submission, not rollout, and survives submission.End().
			carrier := tracing.InjectToMap(incoming)
			if carrier["traceparent"] == "" || stored.GetAnnotations()[tracing.AnnotationPrefix+"generation"] != "1" {
				t.Fatal(stored.Object)
			}
			remote := demo.Incoming(stored, 1)
			if remote["traceparent"] != carrier["traceparent"] {
				t.Fatal("generation binding rejected current operation")
			}
			if scenario == "timeout" {
				// A rollout timeout does not cancel or mutate the Kubernetes desired state.
				current, _ := client.Resource(demo.FooGVR).Namespace("default").Get(context.Background(), "test", metav1.GetOptions{})
				if !demo.Eligible(current, 1) {
					t.Fatal("timeout cancelled controller work")
				}
			}
			rollout.End()
			if len(recorder.Ended()) != 2 {
				t.Fatal("rollout never ended")
			}
		})
	}
}

func addReadyChildren(t *testing.T, client *fake.FakeDynamicClient, root *unstructured.Unstructured) {
	t.Helper()
	parent := root
	for stage := 2; stage <= 3; stage++ {
		name := "bar"
		if stage == 3 {
			name = "baz"
		}
		child := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "example.com/v1", "kind": demo.Kind(stage)}}
		child.SetName(name)
		child.SetNamespace("default")
		child.SetGeneration(1)
		child.SetUID(types.UID(name + "-uid"))
		controller := true
		child.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: parent.GetAPIVersion(), Kind: parent.GetKind(), Name: parent.GetName(), UID: parent.GetUID(), Controller: &controller}})
		child.SetAnnotations(map[string]string{tracing.AnnotationPrefix + "root-uid": string(root.GetUID()), tracing.AnnotationPrefix + "root-generation": "1"})
		_ = unstructured.SetNestedField(child.Object, int64(1), "status", "observedGeneration")
		if stage == 2 {
			_ = unstructured.SetNestedField(child.Object, "baz", "status", "childName")
		}
		if err := client.Tracker().Create(demo.Resource(stage), child, "default"); err != nil {
			t.Fatal(err)
		}
		parent = child
	}
}
