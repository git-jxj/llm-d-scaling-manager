package warmpool

import (
	"context"
	"testing"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/pool"
)

// deniedReport runs ReportBorrowDenied against the given objects and registry,
// at the shipped verbosity, and returns what it logged.
func deniedReport(t *testing.T, entries []registry.Entry, listErr error, objs ...client.Object) *observer.ObservedLogs {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...)
	if listErr != nil {
		b = b.WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return listErr
			},
		})
	}
	core, logs := observer.New(zapcore.Level(-logging.DEFAULT))
	ReportBorrowDenied(context.Background(), zapr.NewLogger(zap.New(core)), b.Build(),
		func() []registry.Entry { return entries }, "workload")
	return logs
}

func poolDeployment(namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "warm-pool", Namespace: namespace,
		Labels: map[string]string{pool.ComponentLabel: pool.ComponentValue},
	}}
}

func errorsIn(logs *observer.ObservedLogs) int {
	return logs.FilterLevelExact(zapcore.ErrorLevel).Len()
}

// A namespace-scoped install that never declared a pool has no reason to grant
// patch on Pods. It used to log an ERROR with remediation on every start.
func TestADenialWithNoPoolDeclaredIsOneInfoLine(t *testing.T) {
	// A plain variant in this namespace, a pool Deployment in ANOTHER: neither
	// declares a pool here.
	plain := registry.Entry{Namespace: "workload", Name: "model-a", Metadata: map[string]string{"modelID": "a"}}
	logs := deniedReport(t, []registry.Entry{plain}, nil, poolDeployment("elsewhere"))

	if n := errorsIn(logs); n != 0 {
		t.Fatalf("no pool is declared, so nothing is wrong: want no ERROR, got %d: %v", n, logs.All())
	}
	if n := logs.FilterMessage(NoPoolDeclared).Len(); n != 1 {
		t.Fatalf("want the pool-off line once, got %d: %v", n, logs.All())
	}
	if n := logs.Len(); n != 1 {
		t.Fatalf("want exactly one line, got %d: %v", n, logs.All())
	}
}

// A declared pool with patch denied holds GPUs it can never lend. That stays
// an ERROR with the remediation, whichever way the pool is declared.
func TestADenialWithAPoolDeclaredIsAnError(t *testing.T) {
	cases := map[string]struct {
		entries []registry.Entry
		objs    []client.Object
	}{
		"by a pool Deployment": {objs: []client.Object{poolDeployment("workload")}},
		"by a ScaledObject trigger": {entries: []registry.Entry{{
			Namespace: "workload", Name: "pool",
			Metadata: map[string]string{registry.WarmPoolNameKey: "default"},
		}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			logs := deniedReport(t, tc.entries, nil, tc.objs...)
			if n := logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessage(BorrowDenied).Len(); n != 1 {
				t.Fatalf("want the remediation ERROR once, got %d: %v", n, logs.All())
			}
			if n := logs.FilterMessage(NoPoolDeclared).Len(); n != 0 {
				t.Fatalf("a declared pool must not read as off: %v", logs.All())
			}
		})
	}
}

// Forbidden is not absent. A list this identity may not make cannot tell "no
// pool" from "a pool it cannot see", so it must not take the quiet branch.
func TestADeclarationThatCannotBeReadIsAnErrorNotInfo(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "", nil)
	logs := deniedReport(t, nil, forbidden)

	if n := logs.FilterMessage(NoPoolDeclared).Len(); n != 0 {
		t.Fatalf("an unreadable declaration was reported as no pool: %v", logs.All())
	}
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	if len(errs) != 1 {
		t.Fatalf("want one ERROR saying the check failed, got %d: %v", len(errs), logs.All())
	}
	if got := errs[0].Message; got != "could not check whether a warm pool is declared in this namespace; if one is, "+BorrowDenied {
		t.Errorf("the ERROR must say the check failed and carry the remediation: %q", got)
	}
	if _, ok := errs[0].ContextMap()["error"]; !ok {
		t.Errorf("the failed read must be attached: %v", errs[0].ContextMap())
	}
}
