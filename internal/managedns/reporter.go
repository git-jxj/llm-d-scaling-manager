package managedns

import (
	"context"
	"slices"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// DefaultReportInterval is how often Reporter publishes the managed set.
const DefaultReportInterval = 15 * time.Second

// Sink receives the managed namespaces and the refused ones with why
// (controllerstatus.Publisher.SetNamespaces).
type Sink interface {
	SetNamespaces(managed []string, refused map[string]string)
}

// Registry is the part of registry.Registry the Reporter prunes.
type Registry interface {
	Snapshot() []registry.Entry
	Forget(namespace, name string)
}

// Reporter publishes, every Interval, which namespaces are managed and which
// are refused, and forgets every registered workload in a namespace that is
// not managed: one whose label was removed, or that the selector stopped
// admitting, after KEDA registered it.
type Reporter struct {
	Admission *Admission
	Sink      Sink
	Registry  Registry
	// Interval between reports; DefaultReportInterval when zero.
	Interval time.Duration
	Log      logr.Logger
}

// NeedLeaderElection is true: the status and the registry the engines read
// are the leader's.
func (*Reporter) NeedLeaderElection() bool { return true }

// Start reports until ctx ends. It implements manager.Runnable.
func (r *Reporter) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultReportInterval
	}
	wait.UntilWithContext(ctx, r.Once, interval)
	return nil
}

// Once runs one report. A failed list publishes nothing and forgets nothing:
// an API blip must not drop every workload.
func (r *Reporter) Once(ctx context.Context) {
	managed, refused, err := r.Admission.Report(ctx)
	if err != nil {
		r.Log.Error(err, "Cannot tell which namespaces are managed; keeping the last report")
		return
	}
	if r.Sink != nil {
		r.Sink.SetNamespaces(managed, refused)
	}
	if r.Registry == nil {
		return
	}
	for _, e := range r.Registry.Snapshot() {
		if slices.Contains(managed, e.Namespace) {
			continue
		}
		r.Registry.Forget(e.Namespace, e.Name)
		r.Log.Info("Forgot a workload in a namespace this controller does not manage",
			"namespace", e.Namespace, "scaledObject", e.Name, "reason", refused[e.Namespace])
	}
}
