package steadystate

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/predictedlatency"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// predictionPublisher keeps what the component published.
type predictionPublisher struct {
	published map[string]predictedlatency.Coverage
	forgot    []string
}

func (p *predictionPublisher) Publish(namespace, modelID, objective string, c predictedlatency.Coverage) {
	if p.published == nil {
		p.published = map[string]predictedlatency.Coverage{}
	}
	p.published[namespace+"/"+modelID+"/"+objective] = c
}

func (p *predictionPublisher) Forget(namespace, modelID string) {
	p.forgot = append(p.forgot, namespace+"/"+modelID)
}

// Off by default: no namespace is named, so the collector issues no query
// of the component's, and nothing is observed or forgotten.
func TestPredictedLatency_OffByDefault(t *testing.T) {
	e := &Engine{}
	groups := map[string][]llmdvariant.Variant{"m": {{ObjectMeta: metav1.ObjectMeta{Namespace: "ns"}}}}
	assert.Nil(t, e.predictionNamespaces(groups))
	e.observePredictedLatency(context.Background(), &modelData{namespace: "ns", modelID: "m"})
	e.retainPredictedLatency(nil)
}

// On, every namespace with a model is named, once.
func TestPredictedLatency_NamesEveryNamespaceWhenOn(t *testing.T) {
	e := &Engine{PredictedLatency: predictedlatency.NewWith(&predictionPublisher{})}
	groups := map[string][]llmdvariant.Variant{
		"a": {{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1"}}},
		"b": {{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1"}}},
		"c": {{ObjectMeta: metav1.ObjectMeta{Namespace: "ns2"}}},
		"d": nil,
	}
	assert.Equal(t, map[string]bool{"ns1": true, "ns2": true}, e.predictionNamespaces(groups))
}

// The component reads the replicas the analyzers read, with what the
// collector kept on them: a pod routing dropped is dropped here too.
func TestSameReplicas_TheAnalyzersPods(t *testing.T) {
	kept := []domain.ReplicaMetrics{{PodName: "a", AvgPrefillTimeKnown: true}, {PodName: "b"}, {PodName: "c", AvgPrefillTimeKnown: true}}
	read := []domain.ReplicaMetrics{{PodName: "c"}, {PodName: "a"}}
	got := sameReplicas(kept, read)
	assert.Equal(t, []domain.ReplicaMetrics{kept[0], kept[2]}, got)
	assert.Empty(t, sameReplicas(kept, nil))
}

// Retain forgets the models no longer active, through the engine's prune.
func TestPredictedLatency_RetainForgetsInactiveModels(t *testing.T) {
	pub := &predictionPublisher{}
	e := &Engine{PredictedLatency: predictedlatency.NewWith(pub)}
	e.PredictedLatency.Observe(domain.AnalyzerInput{Namespace: "ns", ModelID: "m"}, nil)
	e.PredictedLatency.Observe(domain.AnalyzerInput{Namespace: "ns", ModelID: "n"}, nil)
	e.retainPredictedLatency(map[string]bool{"ns/n": true})
	assert.Equal(t, []string{"ns/m"}, pub.forgot)
	e.retainPredictedLatency(nil)
	assert.Equal(t, []string{"ns/m", "ns/n"}, pub.forgot)
}
