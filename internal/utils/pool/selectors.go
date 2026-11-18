package pool

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
)

// ListSelectors returns the Pod selector of every InferencePool in namespace,
// read in the API group the controller is configured for (PoolGroupV1 or
// PoolGroupV1Alpha2).
//
// The error is returned as the API server gave it, so a caller can tell a kind
// that is not served (meta.IsNoMatchError: no pool can exist) from a read that
// was refused or failed (a pool may exist and could not be seen).
func ListSelectors(ctx context.Context, reader client.Reader, group, namespace string) ([]map[string]string, error) {
	switch group {
	case PoolGroupV1:
		var list v1.InferencePoolList
		if err := reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		out := make([]map[string]string, 0, len(list.Items))
		for i := range list.Items {
			sel := make(map[string]string, len(list.Items[i].Spec.Selector.MatchLabels))
			for k, v := range list.Items[i].Spec.Selector.MatchLabels {
				sel[string(k)] = string(v)
			}
			out = append(out, sel)
		}
		return out, nil
	case PoolGroupV1Alpha2:
		var list v1alpha2.InferencePoolList
		if err := reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		out := make([]map[string]string, 0, len(list.Items))
		for i := range list.Items {
			sel := make(map[string]string, len(list.Items[i].Spec.Selector))
			for k, v := range list.Items[i].Spec.Selector {
				sel[string(k)] = string(v)
			}
			out = append(out, sel)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown InferencePool group %q", group)
	}
}

// AnySelects reports whether any of the selectors matches labels. An empty
// selector matches every Pod, as a Kubernetes label selector does.
func AnySelects(selectors []map[string]string, labels map[string]string) bool {
	for _, sel := range selectors {
		if IsSubset(sel, labels) {
			return true
		}
	}
	return false
}
