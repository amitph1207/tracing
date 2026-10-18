package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"tracing-demo/internal/demo"
	"tracing-demo/internal/tracing"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdown, err := tracing.Init(ctx, "crd-creator")
	if err != nil {
		return err
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shutdown(flush); err != nil {
			log.Printf("tracing shutdown: %v", err)
		}
	}()
	dyn, err := demo.Client()
	if err != nil {
		return err
	}
	timeout, err := time.ParseDuration(demo.Env("ROLLOUT_TIMEOUT", "60s"))
	if err != nil || timeout <= 0 {
		return fmt.Errorf("ROLLOUT_TIMEOUT must be a positive duration")
	}
	mode := demo.Env("TRACING_MODE", "child")
	if mode != "child" && mode != "linked" {
		return fmt.Errorf("TRACING_MODE must be child or linked")
	}
	contextMode := demo.Env("CONTEXT_MODE", "valid")
	switch contextMode {
	case "valid", "missing", "invalid", "unsampled":
	default:
		return fmt.Errorf("CONTEXT_MODE must be valid, missing, invalid or unsampled")
	}
	spec := map[string]interface{}{"message": demo.Env("MESSAGE", "hello "+strconv.FormatInt(time.Now().UnixNano(), 10))}
	controls := map[string]string{tracing.AnnotationPrefix + "mode": mode}
	for _, input := range []struct{ env, field string }{
		{"DELAY_MILLIS", "delay-millis"}, {"FAIL_STAGE", "fail-stage"}, {"FAIL_ATTEMPTS", "fail-attempts"},
	} {
		value, err := strconv.ParseInt(demo.Env(input.env, "0"), 10, 64)
		if err != nil || value < 0 || (input.field == "fail-stage" && value > 3) || (input.field == "delay-millis" && value > 60000) {
			return fmt.Errorf("invalid %s", input.env)
		}
		controls[tracing.AnnotationPrefix+input.field] = strconv.FormatInt(value, 10)
	}
	resource := dyn.Resource(demo.FooGVR).Namespace(demo.Env("NAMESPACE", "default"))
	name := demo.Env("FOO_NAME", fmt.Sprintf("foo-sample-%d", time.Now().UnixNano()))
	action := demo.Env("ACTION", "create")
	var obj *unstructured.Unstructured
	var options []trace.SpanStartOption
	switch action {
	case "create":
		obj = &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "example.com/v1", "kind": "Foo"}}
		obj.SetName(name)
	case "update":
		obj, err = resource.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		// A new desired-state operation starts a new trace, optionally linked to the old operation.
		previous := trace.SpanContextFromContext(tracing.ExtractAnnotations(context.Background(), obj.GetAnnotations()))
		if previous.IsValid() {
			options = append(options, trace.WithLinks(trace.Link{SpanContext: previous}))
		}
	default:
		return fmt.Errorf("ACTION must be create or update")
	}
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	for k, v := range controls {
		annotations[k] = v
	}
	obj.SetAnnotations(annotations)
	ctx, rollout := otel.Tracer("crd-creator").Start(ctx, "foo.rollout", append(options, trace.WithNewRoot())...)
	defer rollout.End()
	rollout.SetAttributes(attribute.String("k8s.object.name", name), attribute.String("operation.action", action))
	log.Printf("trace id: %s name=%s action=%s", rollout.SpanContext().TraceID(), name, action)
	err = submitAndWait(ctx, dyn, resource, obj, spec, contextMode, action, timeout, rollout)
	if err != nil {
		rollout.RecordError(err)
		rollout.SetStatus(codes.Error, err.Error())
		return err
	}
	rollout.SetAttributes(attribute.String("rollout.outcome", "ready"))
	return nil
}

func submitAndWait(ctx context.Context, client dynamic.Interface, resource dynamic.ResourceInterface, obj *unstructured.Unstructured, spec map[string]interface{}, contextMode, action string, timeout time.Duration, rollout trace.Span) error {
	submitCtx, submit := otel.Tracer("crd-creator").Start(ctx, action+"-foo")
	expectedGeneration := obj.GetGeneration() + 1
	annotations := tracing.InjectAnnotations(submitCtx, obj.GetAnnotations())
	annotations[tracing.AnnotationPrefix+"generation"] = strconv.FormatInt(expectedGeneration, 10)
	annotations[tracing.AnnotationPrefix+"operation-id"] = rollout.SpanContext().TraceID().String()
	switch contextMode {
	case "missing":
		delete(annotations, tracing.AnnotationPrefix+"traceparent")
		delete(annotations, tracing.AnnotationPrefix+"tracestate")
	case "invalid":
		annotations[tracing.AnnotationPrefix+"traceparent"] = "not-a-valid-traceparent"
	case "unsampled":
		sc := trace.SpanContextFromContext(submitCtx).WithTraceFlags(0)
		annotations = tracing.InjectAnnotations(trace.ContextWithSpanContext(submitCtx, sc), annotations)
	}
	obj.SetAnnotations(annotations)
	obj.Object["spec"] = spec
	var created *unstructured.Unstructured
	var err error
	if action == "create" {
		created, err = resource.Create(submitCtx, obj, metav1.CreateOptions{})
	} else {
		created, err = resource.Update(submitCtx, obj, metav1.UpdateOptions{})
	}
	if err != nil {
		submit.RecordError(err)
		submit.SetStatus(codes.Error, err.Error())
	}
	submit.End() // Submission ends at API acknowledgement; rollout continues waiting.
	if err != nil {
		return err
	}
	if created.GetGeneration() != expectedGeneration {
		return fmt.Errorf("spec did not change; use a new MESSAGE for a new generation")
	}
	rollout.SetAttributes(attribute.Int64("k8s.object.generation", created.GetGeneration()), attribute.String("k8s.object.uid", string(created.GetUID())))
	log.Printf("submitted Foo %s/%s uid=%s generation=%d", created.GetNamespace(), created.GetName(), created.GetUID(), created.GetGeneration())
	err = wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, timeout, true, func(pollCtx context.Context) (bool, error) {
		current, err := resource.Get(pollCtx, created.GetName(), metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if current.GetUID() != created.GetUID() || current.GetGeneration() != created.GetGeneration() {
			rollout.SetAttributes(attribute.String("rollout.outcome", "superseded"))
			return false, fmt.Errorf("rollout superseded by a replacement object or newer generation")
		}
		return demo.ChainReady(pollCtx, client, current)
	})
	if err != nil {
		if ctx.Err() != nil {
			rollout.SetAttributes(attribute.String("rollout.outcome", "cancelled"))
		} else if err == context.DeadlineExceeded {
			rollout.SetAttributes(attribute.String("rollout.outcome", "timeout"))
		}
		return fmt.Errorf("wait for rollout: %w", err)
	}
	log.Printf("ready: %s generation=%d", created.GetName(), created.GetGeneration())
	return nil
}
