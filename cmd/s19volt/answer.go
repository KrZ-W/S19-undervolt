package main

// answer.go — a wizard level's step rule and answer (owner decision 2026-11-25: EFFICIENCY ONLY; the strict
// answer, its input and its columns are gone).
//
// Every measured step is judged by stepPass (unchanged rule, formerly called "strict"): <= 1 chip newly weak vs
// the fresh tune AND total TH/s >= 96 % of the fresh tune AND chips <= target + 6 °C. The answer is the lowest
// passing step (most drive) of the gallop + bisection (search.go). It feeds the table only if it is safe: back at stock voltage,
// no chip that was healthy in the baseline stays weak (latched). A latch raises the answer (search.go); after the
// raise cap the level runs at stock voltage. Old wizard.json files with strict fields still load (ignored).
// Pure functions only (unit-tested in answer_test.go).

import (
	"math"
	"sort"
)

const (
	passBoard = 0.90 // step passes: EVERY board's mcr (Braiins' per-chip hashrate vs the clocks, the measure its own
	// stage-4 check uses) >= this. Braiins retunes ~3-4 points under its stored hashrate, set to 90 % before any
	// trimmed step: ~86-87 %. (Owner 2026-11-26: a 22.4 J/TH step failed on a Diff1 per-board share of 91 % while
	// its mcr was 97.1 %: Diff1 per board is +-3-6 % at 500-700 W, so it no longer decides.) Weak chips do not fail
	// a step; they still mark the edge of the search (search.go).
	chipHotMarg  = 6.0  // °C above the chip target that still counts as fine during a step
	dropFr       = 0.60 // a board below this share of its stock rate during a hold aborts the hold
	effConfirmTo = 1.03 // re-confirm after a clock change: J/TH may be at most 3 % worse than the answer's
	weakFr       = 0.60 // weak chip: work per MHz below this share of its board's median (weakChips)
)

type wizAnswer struct {
	V       float64  `json:"v"`
	Drive   float64  `json:"drive"`
	EstW    float64  `json:"est_w"`
	THs     float64  `json:"ths"`
	JTH     float64  `json:"jth"`
	NewWeak []string `json:"new_weak"` // chips weak at the confirm hold but not in the fresh tune
	Safe    bool     `json:"safe"`
	Step    int      `json:"step"` // index in the level's steps (the confirm hold)
}

// stepPass: the per-step rule. minShare = the lowest board's share of its own stock rate. Weak chips are not part
// of it (owner 2026-11-26): a step with a few weak chips that keeps every board >= passBoard passes. minShare =
// the lowest board's mcr (its share of its stock rate when there is no per-chip data).
func stepPass(minShare, ths, chip, chipTarget float64) bool {
	return ths > 0 && minShare >= passBoard && chip <= chipTarget+chipHotMarg
}

// beatStockFr: a trimmed answer must be at least this much better (lower J/TH) than the level's stock baseline, or
// the level runs at stock voltage (owner 2026-11-26: the tuned 2000 W ran 26.9 J/TH against 26.4 at stock — the
// trim saved 1.8 % power and cost 3.7 % hashrate).
const beatStockFr = 0.01

// beatsStock: the trimmed J/TH is at least beatStockFr better than stock (no stock figure = cannot tell: true).
func beatsStock(jth, stockJTH float64) bool {
	return stockJTH <= 0 || (jth > 0 && jth <= stockJTH*(1-beatStockFr))
}

// usable: a measured step that may become an answer (not discarded by the profile guard, no board drop).
func (s wizStep) usable() bool { return s.Invalid == "" && s.Dropped == "" && s.THs > 0 && s.JTH > 0 }

// answerStep: index of the usable passing step with the best (lowest) estimated J/TH; equal J/TH -> the less drive
// (higher voltage, more margin). The chip rule still decides pass/fail. It was "the most drive" until 2026-11-25:
// below the knee steps keep passing while every chip loses a little hashrate, so the lowest passing voltage was the
// least efficient (run 7, 600 W: 9.81 V 23.04 J/TH chosen over 9.64 V 23.35).
func answerStep(steps []wizStep) int {
	best := -1
	for i, s := range steps {
		if !s.usable() || !s.Pass {
			continue
		}
		if best < 0 || s.JTH < steps[best].JTH || (s.JTH == steps[best].JTH && s.Drive < steps[best].Drive) {
			best = i
		}
	}
	return best
}

// effSafe: the answer is safe only if the latch check ran with per-chip data and nothing stayed latched.
func effSafe(latchKnown bool, latched []string) bool { return latchKnown && len(latched) == 0 }

// newWeakList: chips weak now that were not weak in the fresh tune (sorted, e.g. "2/36").
func newWeakList(now, base map[string]bool) []string {
	out := []string{}
	for k := range now {
		if !base[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// boardDropped: the first enabled board below fr x its stock rate ("" = none). Boards without a reading count
// as 0 TH/s (a board that stopped hashing).
func boardDropped(rates, ref map[string]float64, fr float64) string {
	ids := []string{}
	for b := range ref {
		ids = append(ids, b)
	}
	sort.Strings(ids)
	for _, b := range ids {
		if ref[b] > 0 && rates[b] < fr*ref[b] {
			return b
		}
	}
	return ""
}

// clampTarget: a power target never below Braiins' minimum. Returns the target and whether it was raised.
func clampTarget(t, min int) (int, bool) {
	if min > 0 && t < min {
		return min, true
	}
	return t, false
}

// nextTrim: the next drive value on the way from cur to p, at most step % away.
func nextTrim(cur, p, step float64) float64 {
	if p < cur {
		return math.Max(p, cur-step)
	}
	return math.Min(p, cur+step)
}
