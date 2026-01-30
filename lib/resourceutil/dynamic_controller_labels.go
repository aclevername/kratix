package resourceutil

import (
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// DynamicControllerLabels returns the labels needed by the kro dynamic controller
// child handlers to map child events back to their parent object.
func DynamicControllerLabels(parent *unstructured.Unstructured) map[string]string {
	if parent == nil {
		return nil
	}

	gvk := parent.GroupVersionKind()
	if gvk.Group == "" && gvk.Version == "" && gvk.Kind == "" {
		return nil
	}

	labels := map[string]string{
		metadata.OwnedLabel: "true",
	}
	for k, v := range metadata.NewInstanceLabeler(parent).Labels() {
		labels[k] = v
	}

	return labels
}
