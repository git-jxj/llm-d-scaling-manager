package managedns

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

func namespace(name string, labelled bool, extra map[string]string) *corev1.Namespace {
	l := map[string]string{"kubernetes.io/metadata.name": name}
	if labelled {
		l[config.NamespaceOptInLabel] = "true"
	}
	for k, v := range extra {
		l[k] = v
	}
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
}

func newReader(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), funcs)
}

func selector(s string) func() string { return func() string { return s } }

// The label is required; the selector narrows the labelled set, never widens
// it; an unparsable selector admits nothing.
func TestReason(t *testing.T) {
	cases := []struct {
		name     string
		labels   map[string]string
		selector string
		admitted bool
		says     string
	}{
		{"labelled, no selector", namespace("team-a", true, nil).Labels, "", true, ""},
		{"unlabelled, no selector", namespace("team-a", false, nil).Labels, "", false, "not labelled"},
		{"label false", map[string]string{config.NamespaceOptInLabel: "false"}, "", false, "not labelled"},
		{"labelled, selector by name admits", namespace("team-a", true, nil).Labels,
			config.NamespaceOptInLabel + "=true,kubernetes.io/metadata.name in (team-a,team-b)", true, ""},
		{"labelled, selector by name refuses", namespace("team-c", true, nil).Labels,
			config.NamespaceOptInLabel + "=true,kubernetes.io/metadata.name in (team-a,team-b)", false, "does not admit"},
		{"unlabelled, selector by name names it", namespace("team-a", false, nil).Labels,
			"kubernetes.io/metadata.name in (team-a)", false, "not labelled"},
		{"labelled, admin label required, absent", namespace("team-a", true, nil).Labels, "admin.example/ok=yes", false, "does not admit"},
		{"labelled, admin label required, present", namespace("team-a", true, map[string]string{"admin.example/ok": "yes"}).Labels,
			"admin.example/ok=yes", true, ""},
		{"labelled, selector unparsable", namespace("team-a", true, nil).Labels, "in (", false, "does not parse"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Admission{Selector: selector(c.selector)}
			why := a.Reason(c.labels)
			if (why == "") != c.admitted {
				t.Fatalf("admitted %v, want %v (reason %q)", why == "", c.admitted, why)
			}
			if !strings.Contains(why, c.says) {
				t.Fatalf("reason %q does not say %q", why, c.says)
			}
		})
	}
}

// Admit reads the Namespace: a missing one is refused, a failed read is
// neither admitted nor refused.
func TestAdmit(t *testing.T) {
	boom := errors.New("apiserver blip")
	cl := newReader(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "flaky" {
				return boom
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, namespace("team-a", true, nil), namespace("team-b", false, nil))
	a := &Admission{Reader: cl, Selector: selector("")}
	ctx := context.Background()
	if why, err := a.Admit(ctx, "team-a"); why != "" || err != nil {
		t.Fatalf("team-a: %q, %v", why, err)
	}
	if why, err := a.Admit(ctx, "team-b"); why == "" || err != nil {
		t.Fatalf("team-b (unlabelled) admitted: %q, %v", why, err)
	}
	if why, err := a.Admit(ctx, "gone"); why != "no such namespace" || err != nil {
		t.Fatalf("gone: %q, %v", why, err)
	}
	if why, err := a.Admit(ctx, "flaky"); why != "" || !errors.Is(err, boom) {
		t.Fatalf("flaky: %q, %v", why, err)
	}
}

// Report lists the managed namespaces and the refused ones: labelled ones
// the selector refuses, and unlabelled ones KEDA called for, the latter only
// for RefusedTTL.
func TestReport(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	cl := newReader(t, interceptor.Funcs{},
		namespace("team-a", true, nil), namespace("team-b", true, nil), namespace("team-c", true, nil),
		namespace("tenant-x", false, nil))
	a := &Admission{Reader: cl, Selector: selector("kubernetes.io/metadata.name in (team-a,team-b)"),
		Now: func() time.Time { return now }}
	ctx := context.Background()
	if why, _ := a.Admit(ctx, "tenant-x"); why == "" {
		t.Fatal("tenant-x admitted")
	}
	managed, refused, err := a.Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(managed, []string{"team-a", "team-b"}) {
		t.Fatalf("managed %v", managed)
	}
	if !strings.Contains(refused["team-c"], "does not admit") {
		t.Fatalf("team-c: %q", refused["team-c"])
	}
	if !strings.Contains(refused["tenant-x"], "KEDA called the scaler") {
		t.Fatalf("tenant-x: %q", refused["tenant-x"])
	}
	now = now.Add(RefusedTTL + time.Second)
	if _, refused, _ = a.Report(ctx); refused["tenant-x"] != "" {
		t.Fatalf("tenant-x still refused after RefusedTTL: %q", refused["tenant-x"])
	}
}

type sink struct {
	managed []string
	refused map[string]string
}

func (s *sink) SetNamespaces(managed []string, refused map[string]string) {
	s.managed, s.refused = managed, refused
}

// The Reporter forgets a workload registered in a namespace that is no longer
// managed (its label removed after KEDA registered it) and keeps the rest.
func TestReporterForgetsUnmanaged(t *testing.T) {
	cl := newReader(t, interceptor.Funcs{}, namespace("team-a", true, nil), namespace("team-b", false, nil))
	reg := registry.New(time.Hour)
	reg.Observe("team-a", "model-a", nil)
	reg.Observe("team-b", "model-b", nil)
	s := &sink{}
	r := &Reporter{Admission: &Admission{Reader: cl, Selector: selector("")}, Sink: s, Registry: reg, Log: logr.Discard()}
	r.Once(context.Background())
	if _, ok := reg.Get("team-b", "model-b"); ok {
		t.Fatal("kept a workload in an unlabelled namespace")
	}
	if _, ok := reg.Get("team-a", "model-a"); !ok {
		t.Fatal("forgot a workload in a managed namespace")
	}
	if !slices.Equal(s.managed, []string{"team-a"}) {
		t.Fatalf("published managed %v", s.managed)
	}
}

// A failed list publishes nothing and forgets nothing.
func TestReporterKeepsEverythingOnAFailedList(t *testing.T) {
	cl := newReader(t, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return errors.New("apiserver blip")
		},
	})
	reg := registry.New(time.Hour)
	reg.Observe("team-a", "model-a", nil)
	s := &sink{}
	r := &Reporter{Admission: &Admission{Reader: cl}, Sink: s, Registry: reg, Log: logr.Discard()}
	r.Once(context.Background())
	if _, ok := reg.Get("team-a", "model-a"); !ok {
		t.Fatal("forgot a workload on a failed list")
	}
	if s.managed != nil || s.refused != nil {
		t.Fatal("published on a failed list")
	}
}
