package collector

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/common/model"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The cache tiers (registration/cache_tiers.go), through the real Prometheus
// parse path: each is read when present, and a missing series, a NaN, a
// failed query or another model's series leaves it absent, never 0.

// sourceSamples is one series per source for the test pod, the model named
// modelName.
func sourceSamples(modelName string, bySource map[string]float64) model.Vector {
	vec := make(model.Vector, 0, len(bySource))
	for s, v := range bySource {
		vec = append(vec, &model.Sample{
			Metric: model.Metric{
				seriesModelLabel: model.LabelValue(modelName),
				"pod":            presencePod,
				"instance":       "10.0.0.1:8000",
				"source":         model.LabelValue(s),
			},
			Value:     model.SampleValue(v),
			Timestamp: model.TimeFromUnixNano(time.Now().UnixNano()),
		})
	}
	return vec
}

func TestCachedTokenRateBySourceThroughPrometheusParse(t *testing.T) {
	q := registration.QueryCachedTokenRateBySource
	cases := []struct {
		name   string
		series map[string]model.Value
		failed map[string]bool
		want   []domain.SourceRate
	}{
		{"present, sorted by source",
			map[string]model.Value{q: sourceSamples("test-model", map[string]float64{"host": 300, "device": 1000, "p2p": 0})},
			nil, []domain.SourceRate{{Source: "device", Rate: 1000}, {Source: "host", Rate: 300}, {Source: "p2p", Rate: 0}}},
		{"a NaN, negative or unlabelled series is dropped",
			map[string]model.Value{q: sourceSamples("test-model", map[string]float64{"host": math.NaN(), "disk": -1, "": 5, "device": 7})},
			nil, []domain.SourceRate{{Source: "device", Rate: 7}}},
		{"another model's series is not this one's",
			map[string]model.Value{q: sourceSamples("other-model", map[string]float64{"device": 3})}, nil, nil},
		{"absent series", nil, nil, nil},
		{"failed query", nil, map[string]bool{q: true}, nil},
	}
	for _, tc := range cases {
		got := collectParsed(t, tc.series, tc.failed, nil).CachedTokenRateBySource
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestExternalPrefixCacheRatesThroughPrometheusParse(t *testing.T) {
	rates := []struct {
		name  string
		query string
		get   func(domain.ReplicaMetrics) (float64, bool)
	}{
		{"hits", registration.QueryExternalPrefixCacheHitTokenRate, func(m domain.ReplicaMetrics) (float64, bool) {
			return m.ExternalPrefixCacheHitTokenRate, m.ExternalPrefixCacheHitTokenRateKnown
		}},
		{"queries", registration.QueryExternalPrefixCacheQueryTokenRate, func(m domain.ReplicaMetrics) (float64, bool) {
			return m.ExternalPrefixCacheQueryTokenRate, m.ExternalPrefixCacheQueryTokenRateKnown
		}},
	}
	for _, r := range rates {
		t.Run(r.name, func(t *testing.T) {
			v, known := r.get(collectParsed(t, map[string]model.Value{r.query: presenceSample(1200)}, nil, nil))
			if v != 1200 || !known {
				t.Errorf("present: got %v %v, want 1200 true", v, known)
			}
			v, known = r.get(collectParsed(t, map[string]model.Value{r.query: presenceSample(0)}, nil, nil))
			if v != 0 || !known {
				t.Errorf("a read zero is a reading: got %v %v", v, known)
			}
			for name, series := range map[string]map[string]model.Value{
				"NaN": {r.query: presenceSample(math.NaN())}, "absent": nil,
			} {
				v, known = r.get(collectParsed(t, series, nil, nil))
				if v != 0 || known {
					t.Errorf("%s: got %v %v, want 0 false", name, v, known)
				}
			}
			v, known = r.get(collectParsed(t, nil, map[string]bool{r.query: true}, nil))
			if v != 0 || known {
				t.Errorf("failed: got %v %v, want 0 false", v, known)
			}
		})
	}
}

// A pod's ranks sum each source's rate and each connector rate; a rank
// without one leaves the pod without it.
func TestMergePodInstances_CacheTiers(t *testing.T) {
	rank := func(hit float64, known bool, src []domain.SourceRate) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: presencePod, ExternalPrefixCacheHitTokenRate: hit,
			ExternalPrefixCacheHitTokenRateKnown: known, CachedTokenRateBySource: src}
	}
	pod := mergePodInstances([]domain.ReplicaMetrics{
		rank(100, true, []domain.SourceRate{{Source: "device", Rate: 10}, {Source: "host", Rate: 1}}),
		rank(50, true, []domain.SourceRate{{Source: "device", Rate: 5}, {Source: "p2p", Rate: 2}}),
	})
	if pod.ExternalPrefixCacheHitTokenRate != 150 || !pod.ExternalPrefixCacheHitTokenRateKnown {
		t.Errorf("hits: got %v %v, want 150 true", pod.ExternalPrefixCacheHitTokenRate, pod.ExternalPrefixCacheHitTokenRateKnown)
	}
	want := []domain.SourceRate{{Source: "device", Rate: 15}, {Source: "host", Rate: 1}, {Source: "p2p", Rate: 2}}
	if !reflect.DeepEqual(pod.CachedTokenRateBySource, want) {
		t.Errorf("sources: got %v, want %v", pod.CachedTokenRateBySource, want)
	}
	pod = mergePodInstances([]domain.ReplicaMetrics{rank(100, true, []domain.SourceRate{{Source: "device", Rate: 10}}), rank(0, false, nil)})
	if pod.ExternalPrefixCacheHitTokenRateKnown || pod.CachedTokenRateBySource != nil {
		t.Errorf("a rank without them: got %v %v, want unknown and nil", pod.ExternalPrefixCacheHitTokenRateKnown, pod.CachedTokenRateBySource)
	}
}
