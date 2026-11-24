package whatif

// Stratified service moments (RankMixture.Stratified, a switch that
// defaults off).
//
// Each rank's mean service and its cs^2 set its utilization, and the
// sojourn is steep in both near the knee. Draws reads them from the rank's
// own few hundred draws of the computed-token histogram. On a heavy-tailed
// law, where a top bucket holds about 1e-4 of the requests at a whole
// uncached prompt, whether those draws catch that bucket is chance: on a
// production P/D recording the busiest rank's mean service moved by about
// 11 % (sd) from one draw set to another, and its cs^2 by more, so a priced
// figure moved with the seed. With the switch, the moments are read once
// per priced rate on a fixed grid of momentDraws strata of the law, and
// each rank draws its requests one per stratum (at least
// minStratifiedDraws a rank), so the figures no longer move with the draws.

// momentDraws is the strata serviceMoments reads the moments on.
const momentDraws = 1 << 16

// minStratifiedDraws is the fewest draws a rank takes with the switch.
const minStratifiedDraws = 4096

// serviceMoments is the service line's mean and cs^2 over momentDraws
// strata of the computed-token histogram.
func (m RankMixture) serviceMoments(hx cumHist) (es, cs2 float64) {
	var s1, s2 float64
	for i := range momentDraws {
		v := m.S0 + hx.ppf((float64(i)+0.5)/momentDraws)/m.V
		s1 += v
		s2 += v * v
	}
	es = s1 / momentDraws
	if !(es > 0) {
		return 0, 0
	}
	return es, max(s2/momentDraws/(es*es)-1, 0)
}
