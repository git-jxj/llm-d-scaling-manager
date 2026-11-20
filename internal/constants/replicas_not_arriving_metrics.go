package constants

// Replicas a variant asked for that have not arrived (issue #224).
const (
	// LLMSMReplicasNotArriving is a gauge: how many replicas a variant's
	// scale target asks for beyond those Ready, once that shortfall has
	// lasted longer than twice the role's start time (at least five
	// minutes). The reason label says why, read from the variant's pods
	// (variantmeta.NotArriving*). Absent while the variant has what it asked
	// for, or the shortfall is younger than that.
	// Labels: namespace, variant_name, role, reason (controller_instance when set)
	LLMSMReplicasNotArriving = "llmsm_replicas_not_arriving"
)

// Kubernetes event reasons for replicas that do not arrive. Both are recorded
// on the variant's ScaledObject.
const (
	// K8SEventReplicasNotArriving is a Warning: a variant has fewer Ready
	// replicas than it asked for, for longer than its start time, and why.
	K8SEventReplicasNotArriving = "ReplicasNotArriving"
	// K8SEventReplicasArrived is a Normal event after a
	// K8SEventReplicasNotArriving: the variant has what it asked for again.
	K8SEventReplicasArrived = "ReplicasArrived"
)
