package collector

import (
	"math"
	"reflect"
	"testing"

	"github.com/prometheus/common/model"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/registration"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// The KV transfers (registration/kv_transfers.go), through the real
// Prometheus parse path: a kind is read when both its rates are, and a
// missing series, a NaN or a failed query on either leaves the kind out,
// never a zero.
func TestKVTransferRatesThroughPrometheusParse(t *testing.T) {
	load, loadT := registration.QueryKVOffloadLoadByteRate, registration.QueryKVOffloadLoadSecondsRate
	read, readT := registration.QueryKVTierReadByteRate, registration.QueryKVTierReadSecondsRate
	nixl, nixlT := registration.QueryNIXLByteRate, registration.QueryNIXLSecondsRate
	cases := []struct {
		name   string
		series map[string]model.Value
		failed map[string]bool
		want   []domain.KVTransferRate
	}{
		{"every kind, sorted by kind",
			map[string]model.Value{load: presenceSample(4e9), loadT: presenceSample(0.5), read: presenceSample(1e9),
				readT: presenceSample(0.25), nixl: presenceSample(2e9), nixlT: presenceSample(0.1)},
			nil, []domain.KVTransferRate{{Kind: domain.KVTransferNIXL, ByteRate: 2e9, SecondsRate: 0.1},
				{Kind: domain.KVTransferOffloadLoad, ByteRate: 4e9, SecondsRate: 0.5},
				{Kind: domain.KVTransferTierRead, ByteRate: 1e9, SecondsRate: 0.25}}},
		{"a read zero is a reading",
			map[string]model.Value{load: presenceSample(0), loadT: presenceSample(0)},
			nil, []domain.KVTransferRate{{Kind: domain.KVTransferOffloadLoad}}},
		{"bytes without seconds is no reading",
			map[string]model.Value{load: presenceSample(4e9)}, nil, nil},
		{"a NaN seconds rate is no reading",
			map[string]model.Value{load: presenceSample(4e9), loadT: presenceSample(math.NaN())}, nil, nil},
		{"a failed seconds query is no reading",
			map[string]model.Value{load: presenceSample(4e9)}, map[string]bool{loadT: true}, nil},
		{"absent", nil, nil, nil},
	}
	for _, tc := range cases {
		got := collectParsed(t, tc.series, tc.failed, nil).KVTransferRates
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A pod's ranks sum each kind's rates; a kind a rank lacks is left out of
// the pod.
func TestMergePodInstances_KVTransfers(t *testing.T) {
	rank := func(tr ...domain.KVTransferRate) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: presencePod, KVTransferRates: tr}
	}
	pod := mergePodInstances([]domain.ReplicaMetrics{
		rank(domain.KVTransferRate{Kind: domain.KVTransferOffloadLoad, ByteRate: 1e9, SecondsRate: 0.1},
			domain.KVTransferRate{Kind: domain.KVTransferNIXL, ByteRate: 5, SecondsRate: 1}),
		rank(domain.KVTransferRate{Kind: domain.KVTransferOffloadLoad, ByteRate: 3e9, SecondsRate: 0.3}),
	})
	want := []domain.KVTransferRate{{Kind: domain.KVTransferOffloadLoad, ByteRate: 4e9, SecondsRate: 0.4}}
	if !reflect.DeepEqual(pod.KVTransferRates, want) {
		t.Errorf("got %v, want %v", pod.KVTransferRates, want)
	}
	pod = mergePodInstances([]domain.ReplicaMetrics{rank(domain.KVTransferRate{Kind: domain.KVTransferTierRead, ByteRate: 1, SecondsRate: 1}), rank()})
	if pod.KVTransferRates != nil {
		t.Errorf("a rank without them: got %v, want nil", pod.KVTransferRates)
	}
}
