// Package demo defines the durable handoff shared by the three demo controllers.
package demo

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"tracing-demo/internal/tracing"
)

var FooGVR = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "foos"}

func Client() (dynamic.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.BuildConfigFromFlags("", Env("KUBECONFIG", os.Getenv("HOME")+"/.kube/config"))
	}
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(cfg)
}

func Env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func StageName(stage int) string { return fmt.Sprintf("controller%d", stage) }

// OperationID avoids labeling a manual spec edit as the previous creator operation.
func OperationID(u *unstructured.Unstructured) string {
	if u.GetAnnotations()[tracing.AnnotationPrefix+"generation"] == strconv.FormatInt(u.GetGeneration(), 10) {
		if id := u.GetAnnotations()[tracing.AnnotationPrefix+"operation-id"]; id != "" {
			return id
		}
	}
	return fmt.Sprintf("%s/generation/%d", u.GetUID(), u.GetGeneration())
}
func Number(u *unstructured.Unstructured, fields ...string) int64 {
	v, _, _ := unstructured.NestedInt64(u.Object, fields...)
	return v
}
func Text(u *unstructured.Unstructured, fields ...string) string {
	v, _, _ := unstructured.NestedString(u.Object, fields...)
	return v
}

// Resource selects the kind owned by each controller; spec is only a message string.
func Resource(stage int) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: []string{"foos", "bars", "bazs"}[stage-1]}
}
func Kind(stage int) string { return []string{"Foo", "Bar", "Baz"}[stage-1] }
func Setting(u *unstructured.Unstructured, name string) string {
	return u.GetAnnotations()[tracing.AnnotationPrefix+name]
}
func SettingNumber(u *unstructured.Unstructured, name string) int64 {
	value, _ := strconv.ParseInt(Setting(u, name), 10, 64)
	return value
}

// Each desired-state generation gets a distinct downstream object. Retries reuse it.
func ChildName(u *unstructured.Unstructured) string {
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", u.GetUID(), u.GetGeneration())))
	name := u.GetName()
	if len(name) > 240 {
		name = strings.TrimRight(name[:240], "-.")
	}
	return fmt.Sprintf("%s-%x", name, hash[:6])
}
func Eligible(u *unstructured.Unstructured, stage int) bool {
	return u.GetDeletionTimestamp() == nil && Number(u, "status", "observedGeneration") != u.GetGeneration()
}
func Incoming(u *unstructured.Unstructured, stage int) map[string]string {
	if Setting(u, "generation") != strconv.FormatInt(u.GetGeneration(), 10) {
		return nil
	}
	return tracing.InjectToMap(tracing.ExtractAnnotations(context.Background(), u.GetAnnotations()))
}
func OwnedBy(child, parent *unstructured.Unstructured) bool {
	for _, ref := range child.GetOwnerReferences() {
		if ref.APIVersion == parent.GetAPIVersion() && ref.Kind == parent.GetKind() && ref.Name == parent.GetName() && ref.UID == parent.GetUID() && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// ChainReady follows the current generation's handoffs; an old child cannot satisfy a new rollout.
func ChainReady(ctx context.Context, client dynamic.Interface, root *unstructured.Unstructured) (bool, error) {
	current := root
	for stage := 1; stage <= 3; stage++ {
		if current.GetDeletionTimestamp() != nil || Number(current, "status", "observedGeneration") != current.GetGeneration() {
			return false, nil
		}
		if stage == 3 {
			return true, nil
		}
		name := Text(current, "status", "childName")
		if name == "" {
			return false, nil
		}
		child, err := client.Resource(Resource(stage+1)).Namespace(current.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if !OwnedBy(child, current) || Setting(child, "root-uid") != string(root.GetUID()) || Setting(child, "root-generation") != strconv.FormatInt(root.GetGeneration(), 10) {
			return false, fmt.Errorf("child %s does not belong to current rollout", name)
		}
		current = child
	}
	return false, nil
}
