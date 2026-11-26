package main

// search.go — the ORDER of a level's steps (owner decisions 2026-11-25): gallop + bisection, IN DRIVE SPACE. A step
// is a trim drive %: glide there at the shared pace, hold, judge — no corrections on the board voltage (its readings
// refresh only every ~10 s per board, staggered, and single readings swing +/-0.3-0.4 V: a correction loop chased
// them and landed up to 0.12 V off its aim). The step's voltage is the hold's MEAN board voltage (per board and
// overall), recorded, never steered on. Every step is still measured and judged exactly as before
// (levelRun.measure / hold / judge: short checks far above the prediction that extend to the full 5 min when they
// look off, the per-chip rule stepPass). Pure logic only (unit-tested in search_test.go); the wizard (wizard.go
// searchLevel) runs the steps.
//
//	first     startDrive(predicted voltage) (first level of a board set: the reference table; later levels: the
//	          previous answer of the same board set + the reference miner's slope between the two wattages)
//	gallop    a fail -> LESS drive (higher V) by the step until a pass; a pass -> MORE drive (lower V) by half the
//	          step until a fail. Step = gallopStep1 for the first level of a board set, gallopStepNext once a
//	          previous answer exists. The step sizes keep their meaning in volts: driveStep converts them to drive at
//	          the local slope (startDrive's points/curve around the last measured voltage), clamped to
//	          [minDriveStep, maxDriveStep]
//	bisect    between the most drive that passed and the least drive that failed until they are <= the drive
//	          equivalent of bisectRes apart (or <= minDriveStep)
//	no-cross  never a drive at or beyond a failed drive, never at or below a passed one once bracketed
//	answer    the passing step with the best est. J/TH (answerStep, efficiency only): its drive AND its mean voltage
//	resets    after a failing step the boards get a soft reset (cleanApply) before the next step (none after the last)
//	latch     the check must test the ANSWER (wizard.go latchStart / latchVerdict): if the last step ran at or
//	          below the answer's voltage, straight back to stock and check (clean = answer cleared; latched at the
//	          answer = it latches; latched below it = inconclusive -> reset, 5 min at the answer, check); otherwise
//	          5 min at the answer first (a reset before it if a lower step ran since the last one). Latched -> soft
//	          reset, answer drive - the drive equivalent of latchRaise, measure it again, check again; after
//	          maxRaises the level runs at stock voltage with a note. The search's last failing step is not reset
//	          (the check may start from it)
//
// Every search parameter is in the const block below (one place).

import (
	"fmt"
	"math"
	"strings"
)

const (
	gallopStep1    = 0.20 // V per gallop step for the first level of a board set (converted to drive at the local slope)
	gallopStepNext = 0.10 // V per gallop step when the prediction comes from a previous answer (within ~0.1 V)
	bisectRes      = 0.05 // bisection ends when the bracket is this many volts of drive wide (or minDriveStep)
	// A failed step costs the step plus a soft reset (~1.8x a passing step). Simulated over 20k random floors
	// (+/-0.3 V and +/-0.15 V around the prediction): a SMALLER step down after a pass saves time (it avoids stepping
	// into a failure); a BIGGER step up after a fail costs time (wider bracket = one more halving step).
	// Best: up = Step, down = Step/2 (0.20 / 0.10 V on a first level: 34.2 vs 36.8 min; 30.3 vs 34.0 min).
	gallopUpX   = 1.0
	gallopDownX = 0.5
	// drive steps: the volt sizes above at the local slope, clamped (a flat or noisy stretch of the points must not
	// give a zero or a huge move); fallbackVPerPct when no slope is known
	minDriveStep    = 0.3
	maxDriveStep    = 5.0
	fallbackVPerPct = 0.07 // V per drive % in the mod's useful range (sweep: ~3 V over ~45 %)
	maxSteps        = 10   // measured steps per level at most (bounds the time; the best pass so far is kept)
	latchRaise      = 0.05 // V added to the answer after a latch (as drive at the local slope)
	maxRaises       = 4    // latch raises per level, then the level runs at stock voltage
	confirmUp       = 0.05 // clock landing: the re-confirm's one step up when the scaled clocks need it (as drive)
	shortMin        = 2    // minutes of a short check (steps more than shortAbove over the prediction); one that looks off is extended to holdMin
	shortAbove      = 0.20 // V above the prediction where steps start as short checks (the drive of pred + shortAbove)
	stockGap        = 0.10 // the least drive allowed gives this far below the stock voltage
)

func r2(v float64) float64 { return math.Round(v*100) / 100 }

// driveEq: the drive % that lowers the voltage by dv from v, from driveAt (voltage -> drive: startDrive at this
// level, its measured points first, then the sweep curve) over [v-dv, v]. A flat or inverted stretch, or no voltage
// known, falls back to fallbackVPerPct. Not clamped (driveStep clamps).
func driveEq(driveAt func(float64) float64, v, dv float64) float64 {
	if v > 0 && driveAt != nil {
		if d := driveAt(v-dv) - driveAt(v); d > 0 && !math.IsInf(d, 0) {
			return d
		}
	}
	return dv / fallbackVPerPct
}

// driveStep: driveEq clamped to [minDriveStep, maxDriveStep].
func driveStep(driveAt func(float64) float64, v, dv float64) float64 {
	return math.Max(minDriveStep, math.Min(maxDriveStep, driveEq(driveAt, v, dv)))
}

// drivePlan: the gallop + bisection state of one level, in drive % (more drive = lower voltage).
type drivePlan struct {
	Step       float64               // gallop step in V (gallopStep1 / gallopStepNext)
	MinD, MaxD float64               // least drive (highest V: stockGap below stock) and most drive (lowest allowed V)
	LowV       float64               // lowest allowed voltage: a step whose mean is at or below it caps MaxD there
	DriveAt    func(float64) float64 // voltage -> drive at this level (the local slope for driveStep)
	Pass       float64               // most drive that passed (valid when HasPass)
	Fail       float64               // least drive that failed (valid when HasFail)
	HasPass    bool
	HasFail    bool
	LastV      float64 // the last measured mean voltage (where the slope is read)
	N          int     // steps recorded
}

// first: the first step (the predicted voltage's drive, inside [MinD, MaxD]).
func (p *drivePlan) first(d float64) float64 { return r2(math.Max(p.MinD, math.Min(p.MaxD, d))) }

// next records the step at drive d (mean voltage v, 0 = unknown) and returns the next drive, or done. The answer is
// p.Pass (when HasPass).
func (p *drivePlan) next(d, v float64, pass bool) (float64, bool) {
	const eps = 1e-6
	p.N++
	if pass {
		if !p.HasPass || d > p.Pass {
			p.Pass = d
		}
		p.HasPass = true
	} else {
		if !p.HasFail || d < p.Fail {
			p.Fail = d
		}
		p.HasFail = true
	}
	if v > 0 {
		p.LastV = v
		if v <= p.LowV+eps && d < p.MaxD { // measured at the lowest allowed voltage: no more drive than this
			p.MaxD = d
		}
	}
	if p.N >= maxSteps {
		return 0, true
	}
	var nd float64
	switch {
	case p.HasPass && p.HasFail: // bracketed: bisect
		res := driveStep(p.DriveAt, p.LastV, bisectRes)
		if p.Fail-p.Pass <= res+eps {
			return 0, true
		}
		nd = r2((p.Pass + p.Fail) / 2)
	case p.HasPass: // only passes so far: more drive (lower V) by half the step
		if p.MaxD-p.Pass < minDriveStep-eps { // the most drive allowed itself passed
			return 0, true
		}
		nd = r2(math.Min(p.MaxD, p.Pass+driveStep(p.DriveAt, p.LastV, p.Step*gallopDownX)))
	default: // only fails: less drive (higher V) by the step
		if p.Fail <= p.MinD+eps {
			return 0, true // failed just below stock: no trimmed answer
		}
		nd = r2(math.Max(p.MinD, p.Fail-driveStep(p.DriveAt, p.LastV, p.Step*gallopUpX)))
	}
	if (p.HasFail && nd >= p.Fail-eps) || (p.HasPass && nd <= p.Pass+eps) { // never cross the bracket
		return 0, true
	}
	return nd, false
}

// latchedAtNote: the stock-fallback note of a level whose every raise latched, e.g.
// "latched at 10.30 V and 10.35 V".
func latchedAtNote(vs []float64) string {
	p := make([]string, len(vs))
	for i, v := range vs {
		p[i] = fmt.Sprintf("%.2f V", v)
	}
	switch len(p) {
	case 0:
		return ""
	case 1:
		return "latched at " + p[0]
	}
	return "latched at " + strings.Join(p[:len(p)-1], ", ") + " and " + p[len(p)-1]
}

// searchOurMin: our own typical minutes per level on top of the Braiins tune: steps (glide ~0.5 + 5-min hold, no
// corrections; the
// stock baseline is measured during the settled-round wait, owner 2026-11-25) + soft resets after fails (cleanMin each, about half the steps fail) + latch window 5 (+ settle).
// First level of a board set: ~4 steps (0.2 V gallop + bisection); later levels: ~3 (0.1 V from a close prediction).
// The glide to a step drive is short (a few % at the setup pace): counted as half a minute per step.
func searchOurMin(first bool) int {
	steps, fails := 3, 1
	if first {
		steps, fails = 4, 2
	}
	return (steps*(2*holdMin+1)+1)/2 + fails*cleanMin + latchMin + 1 // the latch check usually starts from the last step (no re-hold)
}
