package main

import (
	"context"
	"fmt"
	"os"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	"tracing-demo/internal/demo"
	"tracing-demo/internal/tracing"
)

// Fake clients do not assign UID/generation like the API server; model those here.
func labClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
	client.PrependReactor("create", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		obj.SetNamespace(action.GetNamespace())
		obj.SetGeneration(1)
		obj.SetUID(types.UID("uid-" + obj.GetName()))
		if err := client.Tracker().Create(action.GetResource(), obj, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
	return client
}
func testRoot() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "example.com/v1", "kind": "Foo", "spec": map[string]interface{}{"message": "hello"}}}
	obj.SetName("test")
	obj.SetNamespace("default")
	obj.SetGeneration(1)
	obj.SetUID("root-uid")
	return obj
}
func cleanupFile(t *testing.T, u *unstructured.Unstructured) {
	t.Helper()
	path := fmt.Sprintf("/tmp/%s-%s-%s.json", u.GetKind(), u.GetNamespace(), u.GetName())
	t.Cleanup(func() { os.Remove(path) })
}
func TestResourceChainAndRetries(t *testing.T) {
	for _, mode := range []string{"child", "linked"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			defer provider.Shutdown(ctx)
			tracer := provider.Tracer("test")
			sourceCtx, source := tracer.Start(ctx, "submit")
			root := testRoot()
			annotations := tracing.InjectAnnotations(sourceCtx, nil)
			for k, v := range map[string]string{"generation": "1", "mode": mode, "fail-stage": "2", "fail-attempts": "2"} {
				annotations[tracing.AnnotationPrefix+k] = v
			}
			root.SetAnnotations(annotations)
			client := labClient(root)
			source.End()
			names := map[int]string{1: root.GetName()}
			var objects []*unstructured.Unstructured
			for index, stage := range []int{1, 2, 2, 2, 3} {
				name := names[stage]
				current, err := client.Resource(demo.Resource(stage)).Namespace("default").Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				cleanupFile(t, current)
				if stage == 3 {
					observedRoot, getErr := client.Resource(demo.FooGVR).Namespace("default").Get(ctx, names[1], metav1.GetOptions{})
					if getErr != nil {
						t.Fatal(getErr)
					}
					ready, readyErr := demo.ChainReady(ctx, client, observedRoot)
					if readyErr != nil || ready {
						t.Fatal("rollout completed before Baz finished", readyErr)
					}
				}
				err = reconcile(ctx, client, tracer, stage, "default/"+name)
				if index == 1 || index == 2 {
					if err == nil {
						t.Fatal("expected injected failure")
					}
				} else if err != nil {
					t.Fatal(err)
				}
				current, err = client.Resource(demo.Resource(stage)).Namespace("default").Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if index == 0 || index == 3 {
					names[stage+1] = demo.Text(current, "status", "childName")
				}
				if index == 0 || index == 3 || index == 4 {
					objects = append(objects, current)
				}
			}
			root = objects[0]
			ready, err := demo.ChainReady(ctx, client, root)
			if err != nil || !ready {
				t.Fatalf("ready=%v error=%v", ready, err)
			}
			if demo.Number(objects[1], "status", "attempts") != 3 {
				t.Fatal("retry attempts not persisted on Bar")
			}
			for i := 1; i < len(objects); i++ {
				if !demo.OwnedBy(objects[i], objects[i-1]) {
					t.Fatal("missing owner relationship")
				}
				spec, _, _ := unstructured.NestedMap(objects[i].Object, "spec")
				if len(spec) != 1 || spec["message"] != "hello" {
					t.Fatal("spec should contain only message", spec)
				}
			}
			var attempts []sdktrace.ReadOnlySpan
			for _, s := range recorder.Ended() {
				if s.Name() != "write-to-disk" && s.Name() != "submit" {
					attempts = append(attempts, s)
				}
			}
			if len(attempts) != 5 {
				t.Fatalf("attempts=%d", len(attempts))
			}
			expected := source.SpanContext()
			for i, attempt := range attempts {
				if mode == "child" {
					if attempt.Parent().SpanID() != expected.SpanID() || attempt.SpanContext().TraceID() != expected.TraceID() {
						t.Fatalf("attempt %d wrong parent", i)
					}
				} else if attempt.Parent().IsValid() || len(attempt.Links()) != 1 || attempt.Links()[0].SpanContext.SpanID() != expected.SpanID() || attempt.SpanContext().TraceID() == expected.TraceID() {
					t.Fatalf("attempt %d wrong link", i)
				}
				if i == 1 || i == 2 {
					if attempt.Status().Code != codes.Error {
						t.Fatal("missing error status")
					}
				}
				if i == 0 || i == 3 {
					expected = attempt.SpanContext()
				}
			}
			// Each child annotation references the controller which actually created it.
			for i, attemptIndex := range []int{0, 3} {
				incoming := tracing.ExtractAnnotations(ctx, objects[i+1].GetAnnotations())
				if tracing.InjectToMap(incoming)["traceparent"] != tracing.InjectToMap(attemptsContext(ctx, attempts[attemptIndex]))["traceparent"] {
					t.Fatal("child context was copied from upstream")
				}
			}
			before := len(recorder.Ended())
			for stage, name := range names {
				if err := reconcile(ctx, client, tracer, stage, "default/"+name); err != nil {
					t.Fatal(err)
				}
			}
			if len(recorder.Ended()) != before {
				t.Fatal("completed resources emitted extra spans")
			}
			oldChild := demo.Text(root, "status", "childName")
			root.SetGeneration(2)
			_ = unstructured.SetNestedField(root.Object, "updated", "spec", "message")
			if _, err := client.Resource(demo.FooGVR).Namespace("default").Update(ctx, root, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			ready, err = demo.ChainReady(ctx, client, root)
			if err != nil || ready {
				t.Fatal("old children satisfied new generation")
			}
			if err := reconcile(ctx, client, tracer, 1, "default/"+root.GetName()); err != nil {
				t.Fatal(err)
			}
			last := recorder.Ended()[len(recorder.Ended())-1]
			if last.Parent().IsValid() || len(last.Links()) != 0 {
				t.Fatal("reused stale annotation")
			}
			root, _ = client.Resource(demo.FooGVR).Namespace("default").Get(ctx, root.GetName(), metav1.GetOptions{})
			if demo.Text(root, "status", "childName") == oldChild {
				t.Fatal("reused previous generation child")
			}
			child, err := client.Resource(demo.Resource(2)).Namespace("default").Get(ctx, demo.Text(root, "status", "childName"), metav1.GetOptions{})
			if err != nil || demo.Setting(child, "root-generation") != "2" || demo.Text(child, "spec", "message") != "updated" {
				t.Fatal("new generation was not propagated", err)
			}
			ready, err = demo.ChainReady(ctx, client, root)
			if err != nil || ready {
				t.Fatal("new Bar acknowledged before its own work")
			}
		})
	}
}

func TestRetryAfterStatusConflictPreservesChildContext(t *testing.T) {
	ctx := context.Background()
	root := testRoot()
	client := labClient(root)
	conflict := true
	client.PrependReactor("update", "foos", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" && conflict {
			conflict = false
			return true, nil, apierrors.NewConflict(demo.FooGVR.GroupResource(), root.GetName(), fmt.Errorf("concurrent write"))
		}
		return false, nil, nil
	})
	provider := sdktrace.NewTracerProvider()
	defer provider.Shutdown(ctx)
	tracer := provider.Tracer("test")
	cleanupFile(t, root)
	if err := reconcile(ctx, client, tracer, 1, "default/test"); !apierrors.IsConflict(err) {
		t.Fatalf("error=%v", err)
	}
	child, err := client.Resource(demo.Resource(2)).Namespace("default").Get(ctx, demo.ChildName(root), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	original := demo.Setting(child, "traceparent")
	if err := reconcile(ctx, client, tracer, 1, "default/test"); err != nil {
		t.Fatal(err)
	}
	child, _ = client.Resource(demo.Resource(2)).Namespace("default").Get(ctx, child.GetName(), metav1.GetOptions{})
	if demo.Setting(child, "traceparent") != original {
		t.Fatal("retry replaced the original child creating context")
	}
}

func attemptsContext(ctx context.Context, span sdktrace.ReadOnlySpan) context.Context {
	return trace.ContextWithSpanContext(ctx, span.SpanContext())
}

func TestForeignChildIsRejected(t *testing.T) {
	ctx := context.Background()
	root := testRoot()
	foreign := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "example.com/v1", "kind": "Bar", "spec": map[string]interface{}{"message": "hello"}}}
	foreign.SetName(demo.ChildName(root))
	foreign.SetNamespace("default")
	foreign.SetGeneration(1)
	foreign.SetUID("foreign-uid")
	client := labClient(root, foreign)
	provider := sdktrace.NewTracerProvider()
	defer provider.Shutdown(ctx)
	cleanupFile(t, root)
	if err := reconcile(ctx, client, provider.Tracer("test"), 1, "default/test"); err == nil {
		t.Fatal("adopted an unrelated child")
	}
	current, _ := client.Resource(demo.FooGVR).Namespace("default").Get(ctx, root.GetName(), metav1.GetOptions{})
	if demo.Number(current, "status", "observedGeneration") == 1 || demo.Text(current, "status", "childName") != "" {
		t.Fatal("published an invalid handoff")
	}
}
