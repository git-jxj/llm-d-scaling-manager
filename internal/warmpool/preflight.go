package warmpool

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/warmpool/pool"
)

// CanBorrow reports whether this controller may do the one thing a borrow
// requires: label a pool Pod into a model's InferencePool.
//
// Without it the pool still ADMITS -- it loads models, holds GPUs, and reports
// itself healthy -- and then fails at the only moment that matters, once per
// spike, in a code path an operator sees only if they are reading logs at the
// time. That is the worst shape a permission error can take, and it is why this
// is asked at startup rather than discovered at the first borrow.
//
// A SelfSubjectAccessReview is the right question because it asks the API server
// what THIS identity may do, rather than reading a Role and re-implementing
// Kubernetes' own aggregation of Roles, ClusterRoles and bindings.
func CanBorrow(ctx context.Context, c client.Client, namespace string) (bool, error) {
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "patch",
				Group:     "",
				Resource:  "pods",
			},
		},
	}
	if err := c.Create(ctx, review); err != nil {
		return false, fmt.Errorf("asking whether this controller may patch pods in %s: %w", namespace, err)
	}
	return review.Status.Allowed, nil
}

// BorrowDenied is what to tell an operator when the answer is no. Written once
// here so the message and the manifest cannot drift apart.
const BorrowDenied = "the warm pool is disabled: this controller may not patch Pods, so it could " +
	"never lend one. Grant patch on pods in the pool namespace -- deploy/warmpool.sh create does, as " +
	"role/scaling-manager-pod-writer -- and restart (a cluster-scoped controller retries on its own). Running " +
	"without it would hold GPUs, warm models, and fail every borrow."

// NoPoolDeclared is what is said when patch on Pods is denied in a namespace
// that declares no warm pool. Nothing is broken there: the pool is simply off.
const NoPoolDeclared = "warm pool off: no warm pool is declared in this namespace"

// ReportBorrowDenied logs a denied borrow review at the level the namespace
// earns.
//
// A namespace-scoped install that never asked for a warm pool has no reason to
// grant patch on Pods, and an ERROR there is noise an operator learns to ignore
// -- which is the wrong lesson for the namespace where a pool IS declared and
// the same denial means GPUs held for nothing. So the denial is an error only
// where a pool is declared, and an info line where none is.
//
// Whether one is declared is read, not assumed. A read that fails is not an
// answer: a Forbidden list cannot tell "no pool" from "a pool this identity may
// not see", so it is reported as an error naming both the failed check and the
// remediation, never folded into the quiet branch.
//
// r should read the API server directly (a manager's APIReader): a cached
// client starts an informer, and a denied informer waits instead of failing.
func ReportBorrowDenied(ctx context.Context, logger logr.Logger, r client.Reader,
	snapshot func() []registry.Entry, namespace string) {
	declared, err := poolDeclared(ctx, r, snapshot, namespace)
	switch {
	case err != nil:
		logger.Error(err, "could not check whether a warm pool is declared in this namespace; if one is, "+
			BorrowDenied, "namespace", namespace)
	case declared:
		logger.Error(nil, BorrowDenied, "namespace", namespace)
	default:
		logger.V(logging.DEFAULT).Info(NoPoolDeclared, "namespace", namespace)
	}
}

// poolDeclared reports whether the namespace declares a warm pool: a
// ScaledObject trigger that names one, or a pool workload -- a Deployment, or a
// LeaderWorkerSet for a multi-host pool. The workloads are read as well as the
// registry because the registry fills as KEDA calls, so at startup it is
// usually still empty, and an unnamed pool from before named pools has no
// trigger at all.
func poolDeclared(ctx context.Context, r client.Reader, snapshot func() []registry.Entry, namespace string) (bool, error) {
	if snapshot != nil {
		for _, entry := range snapshot() {
			if entry.Namespace == namespace && registry.ScalesAWarmPool(entry.Metadata) {
				return true, nil
			}
		}
	}
	inNS := client.InNamespace(namespace)
	labelled := client.MatchingLabels{pool.ComponentLabel: pool.ComponentValue}

	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, inNS, labelled, client.Limit(1)); err != nil {
		return false, fmt.Errorf("list warm pool deployments in %s: %w", namespace, err)
	}
	if len(deployments.Items) > 0 {
		return true, nil
	}

	var sets lwsv1.LeaderWorkerSetList
	err := r.List(ctx, &sets, inNS, labelled, client.Limit(1))
	switch {
	case meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err):
		// No LeaderWorkerSet CRD on this cluster (the scheme registers the
		// type only when the CRD exists), so there is no LWS pool. Anything
		// else, Forbidden included, is a read that did not answer.
		return false, nil
	case err != nil:
		return false, fmt.Errorf("list warm pool leaderworkersets in %s: %w", namespace, err)
	}
	return len(sets.Items) > 0, nil
}
