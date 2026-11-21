package constants

// Replicas a variant asked for that have not arrived (issue #224).
const (
	// LLMSMReplicasNotArriving is a gauge: how many replicas a variant's
	// scale target asks for beyond those Ready, once none has arrived for
	// longer than twice the role's start time (at least five minutes) and
	// the pods behind them are not merely younger than that. The reason label says why, read from the variant's pods
	// (variantmeta.NotArriving*). Absent while the variant has what it asked
	// for, or the shortfall is younger than that.
	// Labels: namespace, variant_name, role, reason (controller_instance when set)
	LLMSMReplicasNotArriving = "llmsm_replicas_not_arriving"
)
