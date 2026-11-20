package whatifapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// weighedResolver is the stub, each source weighing bytes.
type weighedResolver struct {
	stubResolver
	bytes int
}

func (r *weighedResolver) Resolve(ctx context.Context, req InputsRequest) (*Resolved, error) {
	res, err := r.stubResolver.Resolve(ctx, req)
	if err != nil {
		return nil, err
	}
	res.Bytes = r.bytes
	return res, nil
}

// The inputs kept are bounded by their weight, not only by their count: past
// MaxInputsBytes the least used is dropped, and the newest is kept whatever
// it weighs.
func TestInputsCacheIsBoundedByBytes(t *testing.T) {
	// Four sources of other content, so four entries the count bound keeps.
	models := []string{"decode-only", "ctx-16k", "ctx-refused", "ctx-zero"}
	for _, c := range []struct {
		max, each, kept int
	}{
		{max: 1 << 30, each: 40, kept: 4},
		{max: 100, each: 40, kept: 2},
		{max: 100, each: 500, kept: 1},
	} {
		s := New(Config{Resolver: &weighedResolver{bytes: c.each}, WhatifSchema: "schema-v3", MaxInputsBytes: c.max})
		h := s.Handler()
		for _, model := range models {
			code, body := post(t, h, "/v1/inputs", InputsRequest{Model: model, Source: &SourceRequest{Kind: SourceRecording, Name: "run.jsonl.gz"}})
			require.Equal(t, http.StatusOK, code, string(body))
		}
		s.inputs.mu.Lock()
		n, w := s.inputs.order.Len(), s.inputs.weight
		s.inputs.mu.Unlock()
		assert.Equal(t, c.kept, n, "%+v", c)
		assert.Equal(t, c.kept*c.each, w, "%+v", c)
	}
	assert.Equal(t, DefaultInputsBytes, New(Config{Resolver: &stubResolver{}}).inputs.maxWeight, "0 is the default")
}
