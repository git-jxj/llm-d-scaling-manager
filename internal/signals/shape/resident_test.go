package shape

import (
	"math"
	"testing"
)

// At 1000 in / 100 out and a hit of 0.5 the discount is 550 a request and the
// whole context 1050: a reading below the discount is held at it, a reading
// above the context is priced as read, marked past twice the context, and
// held at the context only where the engine over-reads there.
func TestPriceResident(t *testing.T) {
	s := New(1000, 100, 0.5)
	for _, c := range []struct {
		name     string
		measured float64
		held     bool
		want     float64
		how      ResidentPrice
	}{
		{"below the discount", 400, false, 550, ResidentAtDiscount},
		{"between the discount and the context", 750, false, 750, ResidentAsRead},
		{"above the context", 1800, false, 1800, ResidentAsRead},
		{"at twice the context", 2100, false, 2100, ResidentAsRead},
		{"past twice the context", 3150, false, 3150, ResidentAboveContext},
		{"above the context, held", 3150, true, 1050, ResidentHeldAtContext},
		{"inside the context, held", 750, true, 750, ResidentAsRead},
		{"below the discount, held", 400, true, 550, ResidentAtDiscount},
	} {
		got, how := s.PriceResident(c.measured, c.held)
		if math.Abs(got-c.want) > 1e-9 || how != c.how {
			t.Errorf("%s: PriceResident(%v, %v) = %v, %v; want %v, %v", c.name, c.measured, c.held, got, how, c.want, c.how)
		}
	}
}

// The context keeps the spread of output lengths, and a literal's spread of 0
// reads as 1.
func TestContext(t *testing.T) {
	if got := New(1000, 100, 0.5).WithOutputDispersion(2).Context(); math.Abs(got-1100) > 1e-9 {
		t.Errorf("Context with D = 2 = %v, want 1100", got)
	}
	if got := (Shape{AvgInputTokens: 1000, AvgOutputTokens: 100, PrefixHitRate: 0.5}).Context(); math.Abs(got-1050) > 1e-9 {
		t.Errorf("Context of a literal = %v, want 1050", got)
	}
}
