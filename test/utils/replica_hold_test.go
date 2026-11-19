package utils

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// etcdTimeout is the error the API server returned in the CI run that failed
// a passive hold: a 500 with no reason.
var etcdTimeout = &apierrors.StatusError{ErrStatus: metav1.Status{
	Status: metav1.StatusFailure, Code: 500, Message: "etcdserver: request timed out",
}}

// heldReads is n reads one second apart, all showing spec.replicas=3.
func heldReads(n int) []ReplicaRead {
	reads := make([]ReplicaRead, n)
	for i := range reads {
		reads[i] = ReplicaRead{At: time.Duration(i) * time.Second, Spec: 3}
	}
	return reads
}

// failing replaces reads[from:to] with failed reads carrying err.
func failing(reads []ReplicaRead, from, to int, err error) []ReplicaRead {
	for i := from; i < to; i++ {
		reads[i] = ReplicaRead{At: reads[i].At, Spec: -1, Err: err}
	}
	return reads
}

func TestCheckReplicasHeld(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, "d")
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "d", errors.New("rbac"))
	moved := heldReads(73)
	moved[40].Spec = 2
	cases := []struct {
		name    string
		reads   []ReplicaRead
		wantErr string
	}{
		// The PR #168 case1-down window: 73 reads, one etcd timeout at +68s.
		{"ci timeline", failing(heldReads(73), 68, 69, etcdTimeout), ""},
		{"real move", moved, "moved to 2 at +40s"},
		{"all reads failed", failing(heldReads(5), 0, 5, etcdTimeout), "none of 5 reads"},
		// Failures spread out, so no gap is long, but most reads failed.
		{"fewer than half succeeded", alternate(heldReads(73)), "only 25 of 73 reads"},
		{"15s streak mid-window", failing(heldReads(73), 30, 45, etcdTimeout), "for 11s, up to +40s"},
		{"15s streak at the tail", failing(heldReads(73), 58, 73, etcdTimeout), "for 11s, up to +1m8s"},
		{"9s streak is tolerated", failing(heldReads(73), 30, 39, etcdTimeout), ""},
		{"NotFound fails", failing(heldReads(73), 10, 11, notFound), "failed at +10s"},
		{"Forbidden fails", failing(heldReads(73), 10, 11, forbidden), "failed at +10s"},
		{"no reads", nil, "no reads"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckReplicasHeld(c.reads, 3)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("want no error, got %v", err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("want an error containing %q, got none", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Fatalf("want an error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

// alternate fails two reads of every three.
func alternate(reads []ReplicaRead) []ReplicaRead {
	for i := range reads {
		if i%3 != 0 {
			failing(reads, i, i+1, etcdTimeout)
		}
	}
	return reads
}

func TestIsTransientReadError(t *testing.T) {
	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"etcd timeout 500", etcdTimeout, true},
		{"server timeout", apierrors.NewServerTimeout(gr, "get", 1), true},
		{"gateway timeout", apierrors.NewTimeoutError("slow", 1), true},
		{"too many requests", apierrors.NewTooManyRequests("busy", 1), true},
		{"service unavailable", apierrors.NewServiceUnavailable("down"), true},
		{"deadline", context.DeadlineExceeded, true},
		{"connection reset", errors.New("read tcp: connection reset by peer"), true},
		{"not found", apierrors.NewNotFound(gr, "d"), false},
		{"forbidden", apierrors.NewForbidden(gr, "d", errors.New("rbac")), false},
		{"other", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := IsTransientReadError(c.err); got != c.want {
			t.Errorf("%s: IsTransientReadError = %v, want %v", c.name, got, c.want)
		}
	}
}
