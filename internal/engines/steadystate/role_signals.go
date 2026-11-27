package steadystate

import (
	"context"
	"slices"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// resolveRolesFromSignals is variantStates and variantMetadata with each
// variant's role resolved against its pods' engine signals this cycle, as the
// capacity planner resolves a capture's (domain.ResolveRole): a variant whose
// scale target declares a role keeps it, and a log line says when the signals
// read another; one that declares none takes the role the signals read, so an
// unlabelled prefill or decode variant is not sized as serving both. A
// variant whose scale target is not among variants and scaleTargets keeps its
// role: nothing says it was not declared. The slices are copied before any
// change.
func resolveRolesFromSignals(ctx context.Context, modelID string, replicaMetrics []domain.ReplicaMetrics,
	variantStates []domain.VariantReplicaState, variantMetadata []domain.VariantMetadata,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor, variants map[string]*llmdvariant.Variant,
) ([]domain.VariantReplicaState, []domain.VariantMetadata) {
	if len(replicaMetrics) == 0 {
		return variantStates, variantMetadata
	}
	declared := make(map[string]bool, len(variants))
	for _, va := range variants {
		st, ok := scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]
		if !ok {
			continue
		}
		_, labelled := variantmeta.DeclaredRole(st)
		declared[va.Name] = labelled
	}
	signals := domain.RoleFromSignals(replicaMetrics)
	logger := ctrl.LoggerFrom(ctx)
	resolved := map[string]string{}
	for _, vs := range variantStates {
		labelled, known := declared[vs.VariantName]
		if !known {
			continue
		}
		role, note := domain.ResolveRole(vs.VariantName, vs.Role, labelled, signals[vs.VariantName])
		switch {
		case note != "" && labelled:
			logger.Info("A variant's role label disagrees with its engine signals; the label is used",
				"model", modelID, "note", note)
		case note != "":
			logger.V(logging.DEBUG).Info("Variant role from engine signals", "model", modelID, "note", note)
		}
		if role != vs.Role {
			resolved[vs.VariantName] = role
		}
	}
	if len(resolved) == 0 {
		return variantStates, variantMetadata
	}
	variantStates = slices.Clone(variantStates)
	for i := range variantStates {
		if role, ok := resolved[variantStates[i].VariantName]; ok {
			variantStates[i].Role = role
		}
	}
	variantMetadata = slices.Clone(variantMetadata)
	for i := range variantMetadata {
		if role, ok := resolved[variantMetadata[i].VariantName]; ok {
			variantMetadata[i].Role = role
		}
	}
	return variantStates, variantMetadata
}
