package capacity

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The two clocks, tested on the type that owns them.
//
// Every assertion about these was previously indirect, through the saturation
// analyzer's specs: in this package Stale had no direct test at all, and
// Touch, ObservedAt and WriteGapExceeds had none either. That is how the
// collapsed-clock defect survived -- the type's contract was only ever
// exercised in the combinations the analyzer happened to produce, so a change
// that broke the contract but kept those combinations working would pass.
//
// The contract in one sentence: a READ keeps a window alive without making its
// figure newer. Everything below is a corner of that.
//
// Every instant here is an offset from one fixed time, t0. RollingAverage
// takes its clock as an argument, so no spec needs the wall clock, and two
// time.Now reads a scheduler pause apart cannot decide an assertion.
var _ = Describe("RollingAverage's two clocks", func() {
	const (
		gap   = time.Hour
		short = time.Millisecond
	)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	It("starts both clocks together, so a new window is neither stale nor gapped", func() {
		ra := NewRollingAverage(10, t0)
		Expect(ra.StaleAt(t0, short)).To(BeFalse())
		Expect(ra.WriteGapExceedsAt(t0, short)).To(BeFalse())
		Expect(ra.StaleAt(t0.Add(short), short)).To(BeFalse(), "exactly the timeout is not past it")
		Expect(ra.WriteGapExceedsAt(t0.Add(short), short)).To(BeFalse())
		Expect(ra.StaleAt(t0.Add(short+time.Nanosecond), short)).To(BeTrue(),
			"both clocks start at the constructor's instant, not before or after it")
		Expect(ra.WriteGapExceedsAt(t0.Add(short+time.Nanosecond), short)).To(BeTrue())
	})

	It("ages on the clock it is given, not the wall clock", func() {
		ra := NewRollingAverage(10, t0)
		at := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
		ra.TouchAt(at)
		Expect(ra.StaleAt(at.Add(gap), gap)).To(BeFalse(), "exactly the timeout is not past it")
		Expect(ra.StaleAt(at.Add(gap+time.Second), gap)).To(BeTrue())
		Expect(ra.StaleAt(t0, gap)).To(BeTrue(), "any later instant is past it too")
	})

	It("fails safe on a zero value, which the constructor is the only way to avoid", func() {
		// Nothing constructs one this way today (NewRollingAverage is the only
		// construction site). If something ever does, both predicates must
		// report the CONSERVATIVE answer -- evict it, and treat the next
		// observation as a new episode -- rather than silently keeping a
		// window whose timestamps are the zero time.
		var ra RollingAverage
		Expect(ra.StaleAt(t0, gap)).To(BeTrue())
		Expect(ra.WriteGapExceedsAt(t0, gap)).To(BeTrue())
	})

	DescribeTable("what each mutator advances",
		func(apply func(*RollingAverage, time.Time), advancesWrite bool) {
			ra := NewRollingAverage(10, t0)
			ra.AddAt(1, t0) // so RaiseLast has a value to raise
			// Age BOTH clocks, then apply the mutator and see which moved.
			ra.ObservedAt(t0.Add(-2 * gap))
			Expect(ra.StaleAt(t0, gap)).To(BeTrue(), "precondition: both clocks are old")
			Expect(ra.WriteGapExceedsAt(t0, gap)).To(BeTrue())

			apply(ra, t0)

			Expect(ra.StaleAt(t0, gap)).To(BeFalse(),
				"every mutator counts as USE: anything that touches a window means a "+
					"decision still depends on it, and the sweep must not take it")
			Expect(ra.WriteGapExceedsAt(t0, gap)).To(Equal(!advancesWrite))
		},
		Entry("Add is a write", func(ra *RollingAverage, at time.Time) { ra.AddAt(2, at) }, true),
		Entry("RaiseLast is a write", func(ra *RollingAverage, at time.Time) { ra.RaiseLastAt(99, at) }, true),
		Entry("Observe is a write with no new value", func(ra *RollingAverage, at time.Time) { ra.ObservedAt(at) }, true),
		Entry("Touch is NOT a write", func(ra *RollingAverage, at time.Time) { ra.TouchAt(at) }, false),
	)

	It("keeps a read-only window alive forever without making its figure newer", func() {
		// The whole point, and the case the collapsed clock got wrong. A
		// window written once and then only READ -- which is what a bucket on
		// a fleet that has stopped saturating looks like -- must never expire,
		// and must still report the write gap so a new observation resets it
		// instead of blending into a figure from before the gap.
		ra := NewRollingAverage(10, t0)
		ra.AddAt(50000, t0)
		ra.ObservedAt(t0.Add(-30 * 24 * time.Hour))

		for i := 0; i < 50; i++ {
			ra.TouchAt(t0)
		}

		Expect(ra.StaleAt(t0, gap)).To(BeFalse(), "reads keep it; the sweep must not take it")
		Expect(ra.WriteGapExceedsAt(t0, gap)).To(BeTrue(),
			"but nothing has been WRITTEN for a month, and the producer has to be able "+
				"to see that -- collapsed onto one field, the reads hid it and two "+
				"episodes a month apart were averaged together")
		Expect(ra.WriteAgeAt(t0)).To(Equal(30*24*time.Hour),
			"and the age must be reportable, since nothing refuses a window on age any more")
	})

	It("reports an age that tracks the last write, not the last read", func() {
		ra := NewRollingAverage(10, t0)
		ra.AddAt(1, t0)
		ra.ObservedAt(t0.Add(-3 * gap))
		ra.TouchAt(t0)
		Expect(ra.WriteAgeAt(t0)).To(Equal(3*gap),
			"a read must not reset the reported age, or the field is a lie in the log line")
	})

	It("does not disturb the stored values when only the clocks move", func() {
		ra := NewRollingAverage(10, t0)
		ra.AddAt(10, t0)
		ra.AddAt(20, t0)
		before, n := ra.Average(), ra.Len()

		ra.TouchAt(t0)
		ra.ObservedAt(t0)
		ra.ObservedAt(t0.Add(-gap))
		ra.TouchAt(t0.Add(-gap))

		Expect(ra.Average()).To(Equal(before))
		Expect(ra.Len()).To(Equal(n))
	})
})
