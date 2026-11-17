package warmpool

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/pool"
)

// denial is one denied-review scenario: what the registry holds, what the API
// server holds, and how a list of either workload kind fails.
type denial struct {
	entries []registry.Entry
	// noSnapshot passes a nil snapshot func rather than an empty registry.
	noSnapshot bool
	// noLWS is a cluster without the LeaderWorkerSet CRD at startup: the type
	// is left out of the scheme and lwsEnabled is false, as main.go does.
	noLWS bool
	// deploymentErr and lwsErr, when set, are what a list of that kind returns.
	deploymentErr error
	lwsErr        error
	objs          []client.Object
}

// report runs ReportBorrowDenied for the scenario, at the shipped verbosity,
// and returns what it logged and how many LeaderWorkerSet lists it made.
func (d denial) report(t *testing.T) (*observer.ObservedLogs, int) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if !d.noLWS {
		if err := lwsv1.AddToScheme(scheme); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	lwsLists := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d.objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				switch list.(type) {
				case *appsv1.DeploymentList:
					if d.deploymentErr != nil {
						return d.deploymentErr
					}
				case *lwsv1.LeaderWorkerSetList:
					lwsLists++
					if d.lwsErr != nil {
						return d.lwsErr
					}
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	snapshot := func() []registry.Entry { return d.entries }
	if d.noSnapshot {
		snapshot = nil
	}
	core, logs := observer.New(zapcore.Level(-logging.DEFAULT))
	ReportBorrowDenied(context.Background(), zapr.NewLogger(zap.New(core)), c, snapshot, "workload", !d.noLWS)
	return logs, lwsLists
}

var poolLabels = map[string]string{pool.ComponentLabel: pool.ComponentValue}

func poolDeployment(namespace string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "warm-pool", Namespace: namespace, Labels: poolLabels}}
}

func poolLWS(namespace string) *lwsv1.LeaderWorkerSet {
	return &lwsv1.LeaderWorkerSet{ObjectMeta: metav1.ObjectMeta{Name: "warm-pool", Namespace: namespace, Labels: poolLabels}}
}

func forbidden(resource string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", nil)
}

// assertPoolOff fails unless exactly one line was logged: the pool-off info.
func assertPoolOff(t *testing.T, logs *observer.ObservedLogs) {
	t.Helper()
	if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
		t.Fatalf("no pool is declared, so nothing is wrong: want no ERROR, got %d: %v", n, logs.All())
	}
	if n := logs.FilterMessage(NoPoolDeclared).Len(); n != 1 {
		t.Fatalf("want the pool-off line once, got %d: %v", n, logs.All())
	}
	if n := logs.Len(); n != 1 {
		t.Fatalf("want exactly one line, got %d: %v", n, logs.All())
	}
}

// A namespace-scoped install that never declared a pool has no reason to grant
// patch on Pods. It used to log an ERROR with remediation on every start.
func TestADenialWithNoPoolDeclaredIsOneInfoLine(t *testing.T) {
	plain := registry.Entry{Namespace: "workload", Name: "model-a", Metadata: map[string]string{"modelID": "a"}}
	cases := map[string]denial{
		// A plain variant here, pool workloads in ANOTHER namespace: neither
		// declares a pool here.
		"pools only elsewhere": {
			entries: []registry.Entry{plain},
			objs:    []client.Object{poolDeployment("elsewhere"), poolLWS("elsewhere")},
		},
		// The registry is not wired at all.
		"no registry": {noSnapshot: true},
		// The CRD went away after startup. NoMatch arrives wrapped, and is
		// still read as "no such kind".
		"LWS CRD removed after startup, wrapped NoMatch": {lwsErr: fmt.Errorf("listing: %w", &meta.NoKindMatchError{
			GroupKind: schema.GroupKind{Group: lwsv1.GroupVersion.Group, Kind: "LeaderWorkerSet"},
		})},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			logs, _ := d.report(t)
			assertPoolOff(t, logs)
		})
	}
}

// No LeaderWorkerSet CRD at startup is decided from what startup found, not by
// classifying the error a list of an unregistered type returns: that error is
// matched by a bare type assertion, and a wrapped one would read as a failed
// check, an ERROR on every cluster without LeaderWorkerSet.
func TestNoLWSAtStartupIsNeverListed(t *testing.T) {
	notRegistered := runtime.NewNotRegisteredErrForKind("test", lwsv1.GroupVersion.WithKind("LeaderWorkerSetList"))
	d := denial{noLWS: true, lwsErr: fmt.Errorf("listing: %w", notRegistered)}
	logs, lwsLists := d.report(t)
	assertPoolOff(t, logs)
	if lwsLists != 0 {
		t.Errorf("startup found no LeaderWorkerSet CRD, yet LeaderWorkerSets were listed %d times", lwsLists)
	}
}

// A declared pool with patch denied holds GPUs it can never lend. That stays
// an ERROR with the remediation, whichever way the pool is declared.
func TestADenialWithAPoolDeclaredIsAnError(t *testing.T) {
	cases := map[string]denial{
		// The registry is empty, as it usually is at startup: only the
		// workload says a pool exists.
		"registry empty, pool Deployment present":      {objs: []client.Object{poolDeployment("workload")}},
		"registry empty, pool LeaderWorkerSet present": {objs: []client.Object{poolLWS("workload")}},
		"by a ScaledObject trigger": {entries: []registry.Entry{{
			Namespace: "workload", Name: "pool",
			Metadata: map[string]string{registry.WarmPoolNameKey: "default"},
		}}},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			logs, _ := d.report(t)
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
	cases := map[string]denial{
		"Deployments forbidden":      {deploymentErr: forbidden("deployments")},
		"LeaderWorkerSets forbidden": {lwsErr: forbidden("leaderworkersets")},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			logs, _ := d.report(t)
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
		})
	}
}
