package main

import (
	"context"
	"encoding/json"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
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
	stage, err := strconv.Atoi(demo.Env("CONTROLLER_STAGE", "1"))
	if err != nil || stage < 1 || stage > 3 {
		return fmt.Errorf("CONTROLLER_STAGE must be 1, 2 or 3")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	service := demo.StageName(stage)
	shutdown, err := tracing.Init(ctx, service)
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
	queue := workqueue.NewRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(time.Second, 30*time.Second))
	defer queue.ShutDown()
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 30*time.Second, demo.Env("NAMESPACE", "default"), nil)
	informer := factory.ForResource(demo.Resource(stage)).Informer()
	enqueue := func(obj interface{}) {
		if u, ok := obj.(*unstructured.Unstructured); ok && demo.Eligible(u, stage) {
			key, err := cache.MetaNamespaceKeyFunc(u)
			if err == nil {
				queue.Add(key)
			}
		}
	}
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: enqueue, UpdateFunc: func(old, current interface{}) {
			before, okBefore := old.(*unstructured.Unstructured)
			after, okAfter := current.(*unstructured.Unstructured)
			if okBefore && okAfter && (before.GetGeneration() != after.GetGeneration() || before.GetUID() != after.GetUID() || (!demo.Eligible(before, stage) && demo.Eligible(after, stage))) {
				enqueue(current)
			}
		},
	}); err != nil {
		return err
	}
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("cache sync interrupted")
	}
	go func() { <-ctx.Done(); queue.ShutDown() }()
	log.Printf("%s started, watching %s", service, demo.Kind(stage))
	for {
		item, quit := queue.Get()
		if quit {
			return nil
		}
		err := reconcile(ctx, dyn, otel.Tracer(service), stage, item.(string))
		if err != nil && ctx.Err() == nil {
			log.Printf("%s retry %s: %v", service, item, err)
			queue.AddRateLimited(item)
		} else {
			queue.Forget(item)
		}
		queue.Done(item)
	}
}

func reconcile(ctx context.Context, dyn dynamic.Interface, tracer trace.Tracer, stage int, key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	resource := dyn.Resource(demo.Resource(stage)).Namespace(ns)
	u, err := resource.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil || !demo.Eligible(u, stage) {
		return err
	}
	stageName := demo.StageName(stage)
	attempt := int64(1)
	if demo.Number(u, "status", "attemptGeneration") == u.GetGeneration() {
		attempt = demo.Number(u, "status", "attempts") + 1
	}
	incoming := demo.Incoming(u, stage)
	ctx, span := tracing.StartAttempt(ctx, tracer, stageName+".reconcile", demo.Setting(u, "mode"), incoming,
		trace.WithAttributes(
			attribute.String("k8s.namespace.name", ns), attribute.String("k8s.object.name", name),
			attribute.String("k8s.object.kind", u.GetKind()), attribute.String("k8s.object.uid", string(u.GetUID())), attribute.Int64("k8s.object.generation", u.GetGeneration()),
			attribute.Int("controller.stage", stage), attribute.Int64("reconcile.attempt", attempt),
			attribute.String("operation.id", demo.OperationID(u)),
		))
	defer span.End()
	log.Printf("%s %s generation=%d attempt=%d trace_id=%s span_id=%s incoming_traceparent=%s", stageName, key, u.GetGeneration(), attempt, span.SpanContext().TraceID(), span.SpanContext().SpanID(), incoming["traceparent"])
	workErr := work(ctx, tracer, u, stage, attempt)
	progress := map[string]interface{}{
		"attemptGeneration": u.GetGeneration(), "attempts": attempt, "lastTraceID": span.SpanContext().TraceID().String(),
	}
	if workErr == nil && stage < 3 {
		var child *unstructured.Unstructured
		child, workErr = ensureChild(ctx, dyn, u, stage)
		if workErr == nil {
			progress["childName"] = child.GetName()
		}
	}
	if workErr == nil {
		progress["observedGeneration"] = u.GetGeneration()
	} else {
		progress["lastError"] = workErr.Error()
		span.RecordError(workErr)
		span.SetStatus(codes.Error, workErr.Error())
	}
	if err := unstructured.SetNestedMap(u.Object, progress, "status"); err != nil {
		return err
	}
	// resourceVersion protects all stages against concurrent spec/status changes.
	if _, err := resource.UpdateStatus(ctx, u, metav1.UpdateOptions{}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return workErr
}

func work(ctx context.Context, tracer trace.Tracer, u *unstructured.Unstructured, stage int, attempt int64) (err error) {
	_, span := tracer.Start(ctx, "write-to-disk")
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()
	timer := time.NewTimer(time.Duration(demo.SettingNumber(u, "delay-millis")) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	if int64(stage) == demo.SettingNumber(u, "fail-stage") && attempt <= demo.SettingNumber(u, "fail-attempts") {
		return fmt.Errorf("injected failure at stage %d attempt %d", stage, attempt)
	}
	data, err := json.MarshalIndent(u.Object, "", "  ")
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/tmp/%s-%s-%s.json", u.GetKind(), u.GetNamespace(), u.GetName())
	span.SetAttributes(attribute.String("file.path", path))
	return os.WriteFile(path, data, 0644)
}

// ensureChild injects THIS attempt's span into the child in the same write as its spec.
// A retry adopts only its own existing child and preserves the original creating context.
func ensureChild(ctx context.Context, client dynamic.Interface, parent *unstructured.Unstructured, stage int) (*unstructured.Unstructured, error) {
	child := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "example.com/v1", "kind": demo.Kind(stage + 1),
		"spec": map[string]interface{}{"message": demo.Text(parent, "spec", "message")},
	}}
	child.SetName(demo.ChildName(parent))
	child.SetNamespace(parent.GetNamespace())
	controller := true
	child.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: parent.GetAPIVersion(), Kind: parent.GetKind(), Name: parent.GetName(), UID: parent.GetUID(), Controller: &controller}})
	annotations := tracing.InjectAnnotations(ctx, parent.GetAnnotations())
	annotations[tracing.AnnotationPrefix+"generation"] = "1"
	annotations[tracing.AnnotationPrefix+"operation-id"] = demo.OperationID(parent)
	if stage == 1 {
		annotations[tracing.AnnotationPrefix+"root-uid"] = string(parent.GetUID())
		annotations[tracing.AnnotationPrefix+"root-generation"] = strconv.FormatInt(parent.GetGeneration(), 10)
	}
	child.SetAnnotations(annotations)
	resource := client.Resource(demo.Resource(stage + 1)).Namespace(parent.GetNamespace())
	created, err := resource.Create(ctx, child, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		created, err = resource.Get(ctx, child.GetName(), metav1.GetOptions{})
		if err == nil && (!demo.OwnedBy(created, parent) || demo.Text(created, "spec", "message") != demo.Text(parent, "spec", "message") || demo.Setting(created, "root-generation") != annotations[tracing.AnnotationPrefix+"root-generation"] || created.GetDeletionTimestamp() != nil) {
			return nil, fmt.Errorf("existing %s %s is not the expected child", child.GetKind(), child.GetName())
		}
	}
	return created, err
}
