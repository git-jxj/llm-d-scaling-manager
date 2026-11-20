package scaletarget

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// A Static role read on an LWS that does not serve DisaggregatedSetRoleScaler
// (before v0.10.0) still resolves, through the role writer's path, and the
// controller warns once, however many cycles read it.
func TestFetchScaleTarget_WarnsOnceWhenRoleScalerUnserved(t *testing.T) {
	var lines []string
	w := &RoleScalerWarner{}
	log := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
	saved := observeRoleScaler
	observeRoleScaler = func(err error, namespace, target string) bool {
		return w.Observe(log, err, namespace, target)
	}
	t.Cleanup(func() { observeRoleScaler = saved })

	c := fake.NewClientBuilder().WithScheme(dsetScheme()).WithObjects(staticSet(t, "glm", "decode")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == constants.DisaggregatedSetRoleScalerKind {
					return &meta.NoKindMatchError{GroupKind: dsetRoleScalerGVK.GroupKind(), SearchedVersions: []string{"v1"}}
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	for range 3 {
		acc, err := FetchScaleTarget(context.Background(), c, "va", constants.DisaggregatedSetRoleScalerKind, "glm-decode", dsetNS)
		require.NoError(t, err)
		set, role, ok := DisaggregatedSetRole(acc)
		require.True(t, ok)
		assert.Equal(t, []string{"glm", "decode"}, []string{set, role})
	}
	require.Len(t, lines, 1, "%q", lines)
	for _, want := range []string{"WARNING", "v0.10.0 or later", "LWS_CHART_VERSION", "DSET_ROLE_WRITER", "the way it was installed", "through its operator", "glm-decode"} {
		assert.True(t, strings.Contains(lines[0], want), "warning lacks %q: %s", want, lines[0])
	}
}

// Only a NoMatch is the missing kind: NotFound and nil say it is served.
func TestRoleScalerWarner_OnlyNoMatch(t *testing.T) {
	var lines []string
	log := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
	w := &RoleScalerWarner{}
	assert.False(t, w.Observe(log, nil, "ns", "x"))
	assert.False(t, w.Observe(log, assert.AnError, "ns", "x"))
	assert.True(t, w.Observe(log, &meta.NoKindMatchError{GroupKind: dsetRoleScalerGVK.GroupKind()}, "ns", "x"))
	assert.True(t, w.Observe(log, &meta.NoKindMatchError{GroupKind: dsetRoleScalerGVK.GroupKind()}, "ns", "y"))
	assert.Len(t, lines, 1)
}

// The Helm commands in the fix name the chart version of LWSRoleScalerRelease,
// so the two cannot drift, and no namespace the install may not use.
func TestRoleScalerUnservedFix_ChartVersionFollowsRelease(t *testing.T) {
	chart := strings.TrimPrefix(LWSRoleScalerRelease, "v")
	require.NotEqual(t, LWSRoleScalerRelease, chart)
	assert.Equal(t, 2, strings.Count(RoleScalerUnservedFix, "--version "+chart+" "), RoleScalerUnservedFix)
	assert.Equal(t, 2, strings.Count(RoleScalerUnservedFix, "--version "), RoleScalerUnservedFix)
	assert.NotContains(t, RoleScalerUnservedFix, "lws-system")
}
