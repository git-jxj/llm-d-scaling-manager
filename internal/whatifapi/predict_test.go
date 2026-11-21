package whatifapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
)

// btNow is the time the tests' predictions are stamped with.
func btNow() time.Time { return time.Date(2026, 11, 21, 6, 0, 0, 0, time.UTC) }

// btPredict is a prediction of window on fit.
func btPredict(t *testing.T, fit, window *inputsEntry) *Prediction {
	t.Helper()
	cv2 := 1.0
	p, err := predictEntries(context.Background(), fit, window, PredictRequest{ArrivalCV2: &cv2},
		PredictInfo{PlannerVersion: "test", WhatifSchema: "schema-v3", Now: btNow})
	require.NoError(t, err)
	return p
}

// btWindow is the synthetic window, its latency kept or taken out as
// strip-latency takes it out: the router's rate window alone is left.
func btWindow(t *testing.T, r *stubResolver, latency bool) *inputsEntry {
	t.Helper()
	s, l := btSeries(240, 0, 1)
	if !latency {
		for i := range l {
			l[i] = LatencySample{At: l[i].At, Router: &domain.RouterLatency{WindowSeconds: l[i].Router.WindowSeconds}}
		}
	}
	return btEntry(t, r, "window", s, l, btConfig("H100"))
}

// A prediction reads no latency: the window with its latency and without
// it predict the same file, byte for byte.
func TestPredict_SameWithAndWithoutLatency(t *testing.T) {
	r := &stubResolver{}
	fs, fl := btEarlier(btSeries(240, 0, 1))
	fit := btEntry(t, r, "fit", fs, fl, btConfig("H100"))
	with, err := json.Marshal(btPredict(t, fit, btWindow(t, r, true)))
	require.NoError(t, err)
	without, err := json.Marshal(btPredict(t, fit, btWindow(t, r, false)))
	require.NoError(t, err)
	assert.Equal(t, string(with), string(without), "a latency of the window reached the prediction")
	p := btPredict(t, fit, btWindow(t, r, false))
	assert.Equal(t, PredictionKind, p.Kind)
	assert.Equal(t, 300.0, p.MeasureWindow)
	assert.NotEmpty(t, p.InputsHash)
	h, err := predictionHash(*p)
	require.NoError(t, err)
	assert.Equal(t, h, p.PredictionHash)
	for _, s := range p.Steps {
		assert.Zero(t, s.Requests)
		for _, x := range append(append([]BacktestPoint{}, s.TTFT...), s.TPOT...) {
			assert.Nil(t, x.Measured, "a prediction carries no measured figure")
		}
	}
}

// A prediction compared with the same window measured scores as the
// backtest of that window scores it.
func TestCompare_ScoresAsTheBacktest(t *testing.T) {
	r := &stubResolver{}
	fs, fl := btEarlier(btSeries(240, 0, 1))
	fit := btEntry(t, r, "fit", fs, fl, btConfig("H100"))
	measured := btWindow(t, r, true)
	p := btPredict(t, fit, btWindow(t, r, false))
	b, err := json.Marshal(p)
	require.NoError(t, err)
	read, err := ReadPrediction(b)
	require.NoError(t, err)
	got, err := compareEntry(context.Background(), read, measured, CompareRequest{})
	require.NoError(t, err)
	cv2 := 1.0
	want := btRun(t, fit, measured, BacktestRequest{ArrivalCV2: &cv2})
	assert.Equal(t, want.Summary, got.Summary)
	assert.Equal(t, want.Steps, got.Steps)
	assert.Equal(t, want.VerdictLines, got.VerdictLines)
	assert.Equal(t, want.Assumed, got.Assumed)

	// A measured recording that starts before the window and ends after it
	// is compared over the window alone.
	s, l := btSeries(240, 0, 1)
	before, bl := btEarlier(btSeries(240, 0, 1))
	for i := range before {
		before[i].At, bl[i].At = before[i].At.Add(23*time.Hour), bl[i].At.Add(23*time.Hour) // the hour before the window
	}
	wide := btEntry(t, r, "wide", append(before, s...), append(bl, l...), btConfig("H100"))
	got, err = compareEntry(context.Background(), read, wide, CompareRequest{})
	require.NoError(t, err)
	assert.Equal(t, want.Summary, got.Summary)
	assert.Positive(t, got.Summary.Judged)
}

// A prediction edited after it was made, a window that is not the one
// predicted, and a measured window with no latency are refused.
func TestCompare_RefusesAnEditedFileOrAnotherWindow(t *testing.T) {
	r := &stubResolver{}
	fs, fl := btEarlier(btSeries(240, 0, 1))
	fit := btEntry(t, r, "fit", fs, fl, btConfig("H100"))
	p := btPredict(t, fit, btWindow(t, r, false))
	b, err := json.Marshal(p)
	require.NoError(t, err)

	edited := *p
	edited.Steps = append([]BacktestStep{}, p.Steps...)
	s := edited.Steps[3]
	s.TTFT = append([]BacktestPoint{}, s.TTFT...)
	v := *s.TTFT[2].Predicted.Value * 2
	s.TTFT[2].Predicted.Value = &v
	edited.Steps[3] = s
	eb, err := json.Marshal(edited)
	require.NoError(t, err)
	_, err = ReadPrediction(eb)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed after it was made")

	// Rehashed by hand, the edited file reads: the hash shows an edit made
	// with no care to hide it; it does not sign the file.
	edited.PredictionHash, err = predictionHash(edited)
	require.NoError(t, err)
	eb, err = json.Marshal(edited)
	require.NoError(t, err)
	_, err = ReadPrediction(eb)
	require.NoError(t, err, "a figure edited and the file rehashed reads: the window still decides")

	var unknown map[string]any
	require.NoError(t, json.Unmarshal(b, &unknown))
	unknown["extra"] = 1
	ub, err := json.Marshal(unknown)
	require.NoError(t, err)
	_, err = ReadPrediction(ub)
	require.Error(t, err, "an unknown field is refused")

	read, err := ReadPrediction(b)
	require.NoError(t, err)
	// Another window: the same times, another load.
	s2, l2 := btSeries(240, 0, 1)
	for i := range s2 {
		s2[i].Rate *= 1.5
	}
	_, err = compareEntry(context.Background(), read, btEntry(t, r, "other", s2, l2, btConfig("H100")), CompareRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not the one predicted")
	// Another time.
	s3, l3 := btEarlier(btSeries(240, 0, 1))
	_, err = compareEntry(context.Background(), read, btEntry(t, r, "earlier", s3, l3, btConfig("H100")), CompareRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not the window predicted")
	// No latency to compare with.
	_, err = compareEntry(context.Background(), read, btWindow(t, r, false), CompareRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no latency")
}

// POST /v1/predict and /v1/compare answer what the functions do.
func TestPredictAndCompareOverHTTP(t *testing.T) {
	r := &btResolver{}
	h := New(Config{Resolver: r, WhatifSchema: "schema-v3", PlannerVersion: "vtest"}).Handler()
	id := inputsOf(t, h)
	// The stub serves one window: predicting it on itself is refused as
	// its own source; its fit is that window read a day earlier.
	code, body := post(t, h, "/v1/predict", PredictRequest{InputsID: id, WindowInputsID: id})
	assert.Equal(t, http.StatusBadRequest, code, string(body))
	assert.Contains(t, string(body), "own source")

	code, body = post(t, h, "/v1/compare", map[string]any{"inputsId": id, "prediction": map[string]any{"kind": "x"}})
	assert.Equal(t, http.StatusBadRequest, code, string(body))
	assert.Contains(t, string(body), "not a prediction file")

	fs, fl := btEarlier(btSeries(240, 0, 1))
	p := btPredict(t, btEntry(t, &r.stubResolver, "fit", fs, fl, btConfig("H100")), btWindow(t, &r.stubResolver, false))
	pb, err := json.Marshal(p)
	require.NoError(t, err)
	code, body = post(t, h, "/v1/compare", CompareRequest{InputsID: id, Prediction: pb})
	require.Equal(t, http.StatusOK, code, string(body))
	var out BacktestResponse
	require.NoError(t, json.Unmarshal(body, &out))
	assert.Positive(t, out.Summary.Judged)
	assert.True(t, strings.HasPrefix(out.Verdict, "Over the "))
	assert.Contains(t, strings.Join(out.Notes, " "), p.PredictionHash[:12])
}

// A window with no router window predicts over the engines' minute, and a
// file whose points are not the four statistics in order is refused.
func TestPredict_EngineWindowAndPointShape(t *testing.T) {
	r := &stubResolver{}
	fs, fl := btEarlier(btSeries(240, 0, 1))
	fit := btEntry(t, r, "fit", fs, fl, btConfig("H100"))
	s, l := btSeries(240, 0, 1)
	for i := range l {
		l[i].Router = nil
	}
	cv2 := 1.0
	p, err := predictEntries(context.Background(), fit, btEntry(t, r, "w", s, l, btConfig("H100")), PredictRequest{ArrivalCV2: &cv2},
		PredictInfo{Now: btNow})
	require.NoError(t, err)
	assert.Equal(t, 60.0, p.MeasureWindow)

	bad := *btPredict(t, fit, btWindow(t, r, false))
	bad.Steps = append([]BacktestStep{}, bad.Steps...)
	st := bad.Steps[3]
	st.TTFT = append(append([]BacktestPoint{}, st.TTFT...), st.TTFT[0])
	bad.Steps[3] = st
	bad.InputsHash, err = inputsHash(bad.Steps, bad.MeasureWindow)
	require.NoError(t, err)
	bad.PredictionHash, err = predictionHash(bad)
	require.NoError(t, err)
	b, err := json.Marshal(bad)
	require.NoError(t, err)
	_, err = ReadPrediction(b)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "points")
}

// The CSV holds the hashes and a row a step.
func TestPredictionCSV(t *testing.T) {
	r := &stubResolver{}
	fs, fl := btEarlier(btSeries(240, 0, 1))
	p := btPredict(t, btEntry(t, r, "fit", fs, fl, btConfig("H100")), btWindow(t, r, false))
	lines := strings.Split(strings.TrimSpace(PredictionCSV(p)), "\n")
	require.Len(t, lines, 2+len(p.Steps))
	assert.Contains(t, lines[0], p.PredictionHash)
	assert.Contains(t, lines[0], p.InputsHash)
	assert.Equal(t, 13, len(strings.Split(lines[1], ",")))
}
