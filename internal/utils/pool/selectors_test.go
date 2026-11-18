package pool

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	"sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
)

func TestListSelectorsReadsBothGroups(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha2.Install(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&v1.InferencePool{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
			Spec: v1.InferencePoolSpec{Selector: v1.LabelSelector{
				MatchLabels: map[v1.LabelKey]v1.LabelValue{"llm-d.ai/guide": "g"},
			}},
		},
		&v1.InferencePool{
			ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "other"},
			Spec: v1.InferencePoolSpec{Selector: v1.LabelSelector{
				MatchLabels: map[v1.LabelKey]v1.LabelValue{"x": "y"},
			}},
		},
		&v1alpha2.InferencePool{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns"},
			Spec:       v1alpha2.InferencePoolSpec{Selector: map[v1alpha2.LabelKey]v1alpha2.LabelValue{"app": "a"}},
		},
	).Build()

	got, err := ListSelectors(context.Background(), c, PoolGroupV1, "ns")
	if err != nil || len(got) != 1 || got[0]["llm-d.ai/guide"] != "g" {
		t.Fatalf("v1: got %v, %v; want the one pool in ns", got, err)
	}
	got, err = ListSelectors(context.Background(), c, PoolGroupV1Alpha2, "ns")
	if err != nil || len(got) != 1 || got[0]["app"] != "a" {
		t.Fatalf("v1alpha2: got %v, %v", got, err)
	}
	if _, err := ListSelectors(context.Background(), c, "example.com", "ns"); err == nil {
		t.Fatal("an unknown group must be an error, not an empty list")
	}
}

func TestAnySelects(t *testing.T) {
	pod := map[string]string{"llm-d.ai/guide": "g", "app": "a"}
	cases := []struct {
		name string
		sels []map[string]string
		want bool
	}{
		{"no pools", nil, false},
		{"a subset matches", []map[string]string{{"llm-d.ai/guide": "g"}}, true},
		{"a value differs", []map[string]string{{"llm-d.ai/guide": "h"}}, false},
		{"a key is missing", []map[string]string{{"llm-d.ai/role": "decode"}}, false},
		{"any one of several", []map[string]string{{"x": "y"}, {"app": "a"}}, true},
		{"an empty selector matches every Pod", []map[string]string{{}}, true},
	}
	for _, tc := range cases {
		if got := AnySelects(tc.sels, pod); got != tc.want {
			t.Errorf("%s: got %t, want %t", tc.name, got, tc.want)
		}
	}
}
