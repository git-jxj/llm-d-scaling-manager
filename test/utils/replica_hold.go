package utils

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
)

// MaxReplicaReadGap is the longest stretch without a successful read of
// spec.replicas that a hold window tolerates. Reads run once a second; a
// longer gap could hide a move and its revert.
const MaxReplicaReadGap = 10 * time.Second

// ReplicaRead is one read of a Deployment's spec.replicas, taken At into the
// window. Spec is meaningful only when Err is nil.
type ReplicaRead struct {
	At   time.Duration
	Spec int32
	Err  error
}

// IsTransientReadError reports whether a failed API read says nothing about
// the object: a timeout, a 5xx or 429 from the API server, or a dropped
// connection. NotFound, Forbidden and other 4xx errors are not transient.
func IsTransientReadError(err error) bool {
	if err == nil {
		return false
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		code := status.Status().Code
		return code >= http.StatusInternalServerError || code == http.StatusTooManyRequests
	}
	if errors.Is(err, context.DeadlineExceeded) || utilnet.IsProbableEOF(err) ||
		utilnet.IsConnectionReset(err) || utilnet.IsConnectionRefused(err) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// CheckReplicasHeld returns an error unless the reads show spec.replicas held
// at want. A transient read failure is skipped, but every other failure is an
// error, as is a gap of more than MaxReplicaReadGap without a successful read,
// or a window where fewer than half the reads succeeded.
func CheckReplicasHeld(reads []ReplicaRead, want int32) error {
	if len(reads) == 0 {
		return errors.New("no reads of spec.replicas were taken")
	}
	ok := 0
	lastOK := reads[0].At
	for _, r := range reads {
		if r.Err != nil && !IsTransientReadError(r.Err) {
			return fmt.Errorf("reading spec.replicas failed at +%s: %w", r.At, r.Err)
		}
		if gap := r.At - lastOK; gap > MaxReplicaReadGap {
			return fmt.Errorf("no successful read of spec.replicas for %s, up to +%s (limit %s)", gap, r.At, MaxReplicaReadGap)
		}
		if r.Err != nil {
			continue
		}
		ok++
		lastOK = r.At
		if r.Spec != want {
			return fmt.Errorf("spec.replicas moved to %d at +%s, want %d", r.Spec, r.At, want)
		}
	}
	if ok == 0 {
		return fmt.Errorf("none of %d reads of spec.replicas succeeded", len(reads))
	}
	if 2*ok < len(reads) {
		return fmt.Errorf("only %d of %d reads of spec.replicas succeeded", ok, len(reads))
	}
	return nil
}
