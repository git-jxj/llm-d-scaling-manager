package locator_test

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/locator"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// A DisaggregatedSet's pods met on an LWS that does not serve
// DisaggregatedSetRoleScaler (before v0.10.0) warn once, naming the release
// to upgrade to; on one that serves it, nothing is said. A read that answers
// neither (Forbidden) says nothing and is asked again.
func TestResolveScaleTarget_WarnsOnceWhenRoleScalerUnserved(t *testing.T) {
	roleScalerGR := schema.GroupResource{Group: "disaggregatedset.x-k8s.io", Resource: "disaggregatedsetrolescalers"}
	for name, tc := range map[string]struct {
		answer    error
		wantWarns int
		wantReads int
	}{
		"LWS before v0.10.0": {
			answer:    &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "disaggregatedset.x-k8s.io", Kind: constants.DisaggregatedSetRoleScalerKind}, SearchedVersions: []string{"v1"}},
			wantWarns: 1, wantReads: 1,
		},
		"LWS v0.10.0 or later": {answer: apierrors.NewNotFound(roleScalerGR, "glm"), wantWarns: 0, wantReads: 1},
		"read refused":         {answer: apierrors.NewForbidden(roleScalerGR, "glm", nil), wantWarns: 0, wantReads: 3},
	} {
		t.Run(name, func(t *testing.T) {
			ns := testNamespace
			lws, sts, pod := dsetPod(map[string]string{constants.DisaggregatedSetRoleLabelKey: "decode"})
			pod2, pod3 := pod.DeepCopy(), pod.DeepCopy()
			pod2.Name, pod3.Name = pod.Name+"-2", pod.Name+"-3"
			reads := 0
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).
				WithRuntimeObjects(lws, sts, pod, pod2, pod3).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if obj.GetObjectKind().GroupVersionKind().Kind == constants.DisaggregatedSetRoleScalerKind {
							reads++
							return tc.answer
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).Build()
			var lines []string
			log := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
			loc, _ := locator.New(c, nil, locator.WithRoleScalerWarner(&scaletarget.RoleScalerWarner{}, log))

			for _, p := range []string{pod.Name, pod2.Name, pod3.Name} {
				if _, ok, err := loc.ResolveScaleTarget(context.Background(), ns, p); err != nil || !ok {
					t.Fatalf("%s: ok=%v err=%v", p, ok, err)
				}
			}
			if len(lines) != tc.wantWarns {
				t.Fatalf("warnings = %d, want %d: %q", len(lines), tc.wantWarns, lines)
			}
			if reads != tc.wantReads {
				t.Errorf("role scaler reads = %d, want %d", reads, tc.wantReads)
			}
			if tc.wantWarns == 1 {
				for _, want := range []string{"WARNING", "DisaggregatedSetRoleScaler", "v0.10.0 or later", "LWS_CHART_VERSION", "DisaggregatedSet/glm"} {
					if !strings.Contains(lines[0], want) {
						t.Errorf("warning lacks %q: %s", want, lines[0])
					}
				}
			}
		})
	}
}
