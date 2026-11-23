// Package managedns decides which namespaces a controller installed from the
// one release manifest manages: those labelled
// scaling-manager.llmd.ai/managed=true that the namespaceSelector setting also
// admits (proposal PR #404).
//
// A label is not consent: a tenant who can create a namespace can label it.
// The selector is what the admin narrows it with, to names or to a label only
// the admin sets. The scaler refuses every KEDA call from a namespace this
// does not admit, so a workload there is never registered, and the Reporter
// forgets what was registered in a namespace that stopped being admitted.
package managedns

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
)

// ModeLabel is the --namespace-opt-in value that manages labelled namespaces
// only. The flag's empty default keeps every other install as it is.
const ModeLabel = "label"

// RefusedTTL is how long a namespace refused at the scaler stays listed as
// refused after its last refused call.
const RefusedTTL = 10 * time.Minute

// Admission admits or refuses namespaces. Safe for concurrent use.
type Admission struct {
	// Reader reads Namespaces. The manager's cached client is right: the
	// controller already lists and watches namespaces cluster-wide, and the
	// scaler asks on every KEDA call.
	Reader ctrl.Reader
	// Selector returns the namespaceSelector setting in force, "" when unset
	// (config.Config.NamespaceSelector).
	Selector func() string
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu      sync.Mutex
	refused map[string]refusal
}

// refusal is the latest refused call from one namespace.
type refusal struct {
	reason string
	at     time.Time
}

func (a *Admission) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Reason says why a namespace with these labels is not managed, "" when it
// is. An unparsable selector refuses everything: the settings reader rejects
// one and keeps the selector in force before, so this is a guard, not a path.
func (a *Admission) Reason(nsLabels map[string]string) string {
	if nsLabels[config.NamespaceOptInLabel] != "true" {
		return "it is not labelled " + config.NamespaceOptInLabel + "=true"
	}
	text := ""
	if a.Selector != nil {
		text = a.Selector()
	}
	if text == "" {
		return ""
	}
	sel, err := labels.Parse(text)
	if err != nil {
		return fmt.Sprintf("the %s setting %q does not parse: %v", config.SettingNamespaceSelector, text, err)
	}
	if !sel.Matches(labels.Set(nsLabels)) {
		return fmt.Sprintf("the %s setting %q does not admit it", config.SettingNamespaceSelector, text)
	}
	return ""
}

// Admit says whether namespace is managed. refusal is why not, "" when it is.
// err is a Namespace read that failed for another reason than NotFound: the
// caller must neither admit nor refuse on it.
func (a *Admission) Admit(ctx context.Context, namespace string) (refusal string, err error) {
	var ns corev1.Namespace
	switch err := a.Reader.Get(ctx, ctrl.ObjectKey{Name: namespace}, &ns); {
	case apierrors.IsNotFound(err):
		refusal = "no such namespace"
	case err != nil:
		return "", fmt.Errorf("reading namespace %s: %w", namespace, err)
	default:
		refusal = a.Reason(ns.Labels)
	}
	if refusal != "" {
		a.noteRefused(namespace, refusal)
	}
	return refusal, nil
}

func (a *Admission) noteRefused(namespace, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refused == nil {
		a.refused = map[string]refusal{}
	}
	a.refused[namespace] = refusal{reason: reason, at: a.now()}
}

// Report lists the managed namespaces, sorted, and the refused ones with
// why: each labelled namespace the selector does not admit, and each
// namespace the scaler refused a call from within RefusedTTL that is not
// managed now.
func (a *Admission) Report(ctx context.Context) (managed []string, refused map[string]string, err error) {
	var list corev1.NamespaceList
	if err := a.Reader.List(ctx, &list, ctrl.MatchingLabels{config.NamespaceOptInLabel: "true"}); err != nil {
		return nil, nil, fmt.Errorf("listing namespaces labelled %s=true: %w", config.NamespaceOptInLabel, err)
	}
	refused = map[string]string{}
	for i := range list.Items {
		ns := &list.Items[i]
		if why := a.Reason(ns.Labels); why != "" {
			refused[ns.Name] = why
			continue
		}
		managed = append(managed, ns.Name)
	}
	sort.Strings(managed)
	cutoff := a.now().Add(-RefusedTTL)
	a.mu.Lock()
	defer a.mu.Unlock()
	for ns, r := range a.refused {
		switch {
		case r.at.Before(cutoff):
			delete(a.refused, ns)
		case slices.Contains(managed, ns):
		default:
			if _, listed := refused[ns]; !listed {
				refused[ns] = r.reason + " (KEDA called the scaler for it)"
			}
		}
	}
	return managed, refused, nil
}
