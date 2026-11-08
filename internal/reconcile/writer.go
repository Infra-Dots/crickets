package reconcile

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const FieldManager = "crickets"

// Writer owns the label and annotation of a workload. nil maps remove what crickets applied earlier.
type Writer interface {
	Apply(ctx context.Context, kind, namespace, name string, labels, annotations map[string]string) error
}

type KubeWriter struct{ Kube kubernetes.Interface }

// Apply sends a server-side apply of metadata only. Fields left out of the body are released
// by the `crickets` manager, which is how removal works. The pod template is never in the body.
func (w KubeWriter) Apply(ctx context.Context, kind, ns, name string, labels, annotations map[string]string) error {
	meta := map[string]any{"name": name, "namespace": ns}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	body, err := json.Marshal(map[string]any{"apiVersion": "apps/v1", "kind": kind, "metadata": meta})
	if err != nil {
		return err
	}
	opts := metav1.PatchOptions{FieldManager: FieldManager, Force: new(true)}
	switch kind {
	case "Deployment":
		_, err = w.Kube.AppsV1().Deployments(ns).Patch(ctx, name, types.ApplyPatchType, body, opts)
	case "StatefulSet":
		_, err = w.Kube.AppsV1().StatefulSets(ns).Patch(ctx, name, types.ApplyPatchType, body, opts)
	default:
		err = fmt.Errorf("unsupported kind %s", kind)
	}
	return err
}
