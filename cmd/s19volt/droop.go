package main

// droop.go — the preparation's "droop" stage (owner 2026-11-25): the PSU's output line moves with load, so the same
// trim drive gives a different board voltage at a low and a high level (two tuned levels on the first real run:
// 61.5 % -> 9.81 V at 745 W target, 59.9 % -> 9.81 V at 970 W). The sweep runs at one load only, so the drive
// model's load term k (drivemodel.go) had nothing to stand on until a level was measured. This stage measures it
// up front: manual mode at two loads (the user's lowest and highest configured wattage; one uniform clock each at
// Braiins' minimum voltage), three drive points per load, averaged board voltage. The six points join the fit
// (weight of a full hold) so every level's first step lands right, the first level included.
//
// Safety (latch rule: never high clocks at low voltage): the low load goes to the middle of the sweep's steep part
// (never below the sweep's 9.6 V point); the high load only to drives where the sweep still read >= 11.3 V. The
// trim is at 0 before every clock change. A board below 60 % of its rate at drive 0 ends that load's points.

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	droopSettle = 8 * time.Second         // after the trim move (driver glide + capacitor)
	droopReads  = 20                      // board-voltage reads averaged per point
	droopEvery  = 1500 * time.Millisecond // between reads (~30 s per point)
	droopMin    = 8                       // estimate: two manual-mode switches + 6 points
	droopMHzLo  = 100.0                   // sane uniform-clock bounds (MHz) when Braiins' constraints say nothing
	droopMHzHi  = 600.0
	droopLoLim  = 9.6  // V: the low load never goes past the sweep's point at this voltage
	droopHiLim  = 11.3 // V: the high load never goes past the sweep's point at this voltage
)

var (
	droopLoV = []float64{10.9, 10.3, 9.8}    // low load: the middle of the sweep's steep part
	droopHiV = []float64{11.65, 11.5, 11.35} // high load: the top of the curve only (no high clocks at low voltage)
)

// droopClock: the uniform clock that gives about w estimated wall watts at volt (power ∝ clock at fixed voltage:
// the test profile's fixedMHz ≈ bosminer target fixT), clamped to [lo, hi] MHz (lo/hi <= 0: the sane bounds).
// Returns the clock and the load key (bosminer-target equivalent: fixT × clock / fixedMHz, the drive model's load).
func droopClock(w float64, fixT int, volt, lo, hi float64) (mhz float64, load int) {
	if lo <= 0 {
		lo = droopMHzLo
	}
	if hi <= 0 {
		hi = droopMHzHi
	}
	lo, hi = math.Max(lo, droopMHzLo), math.Min(hi, droopMHzHi)
	full := estWall(fixT, volt) - powOverhead // chip watts at the test clock
	mhz = fixedMHz
	if full > 0 {
		mhz = fixedMHz * (w - powOverhead) / full
	}
	mhz = math.Round(math.Max(lo, math.Min(hi, mhz)))
	return mhz, int(math.Round(float64(fixT) * mhz / fixedMHz))
}

// parseFreqRange: a frequency range from a bosminer constraints reply: an object under a key containing "frequency"
// with min / max {hertz} (or {mhz}). Only plausible chip clocks (50..1500 MHz); 0 = not given.
func parseFreqRange(raw json.RawMessage) (lo, hi float64) {
	var v interface{}
	if json.Unmarshal(raw, &v) != nil {
		return 0, 0
	}
	mhzOf := func(x interface{}) float64 {
		m, ok := x.(map[string]interface{})
		if !ok {
			return 0
		}
		f := 0.0
		if h, ok := m["hertz"].(float64); ok {
			f = h / 1e6
		} else if h, ok := m["mhz"].(float64); ok {
			f = h
		}
		if f < 50 || f > 1500 {
			return 0
		}
		return f
	}
	var walk func(x interface{}, inF bool) bool
	walk = func(x interface{}, inF bool) bool {
		switch t := x.(type) {
		case map[string]interface{}:
			if inF {
				a, b := mhzOf(t["min"]), mhzOf(t["max"])
				if a > 0 || b > 0 {
					lo, hi = a, b
					return true
				}
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if walk(t[k], strings.Contains(strings.ToLower(k), "frequency")) {
					return true
				}
			}
		case []interface{}:
			for _, e := range t {
				if walk(e, inF) {
					return true
				}
			}
		}
		return false
	}
	walk(v, false)
	return lo, hi
}

// droopDrives: the measured sweep drives nearest (in voltage) to each target, only among the points before the
// first one that read below limit (a noisy sweep never lets a later point slip past it). Distinct, ascending.
func droopDrives(pts []calPoint, targets []float64, limit float64) []float64 {
	ps := append([]calPoint(nil), pts...)
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Duty < ps[j].Duty })
	var ok []calPoint
	for _, p := range ps {
		if p.V <= 0 {
			continue
		}
		if p.V < limit-1e-9 {
			break
		}
		ok = append(ok, p)
	}
	seen := map[float64]bool{}
	var out []float64
	for _, v := range targets {
		best := -1
		for i, p := range ok {
			if best < 0 || math.Abs(p.V-v) < math.Abs(ok[best].V-v)-1e-9 {
				best = i
			}
		}
		if best >= 0 && !seen[ok[best].Duty] {
			seen[ok[best].Duty] = true
			out = append(out, ok[best].Duty)
		}
	}
	sort.Float64s(out)
	return out
}

// droopNeeded: the run has a usable sweep on the fixed profile but no droop points yet.
func droopNeeded(s wizState) bool {
	if s.Fixed != nil && s.Fixed.Fallback != "" { // manual mode refused on this miner
		return false
	}
	for _, p := range s.Points {
		if p.Src == "droop" {
			return false
		}
	}
	return sweepCurveOf(s) != nil
}

// sweepCurveOf: the run's sweep (the curve with the most points), nil = none.
func sweepCurveOf(s wizState) *wizCurve {
	var c *wizCurve
	for i := range s.Curves {
		if len(s.Curves[i].Points) >= 2 && (c == nil || len(s.Curves[i].Points) >= len(c.Points)) {
			c = &s.Curves[i]
		}
	}
	return c
}

// manualSettle: after a manual-mode switch, every board hashing (gate: load gateT; else the latch recovery), then
// the clocks at mhz (±3 %, both ways) and the PSU quiet (fixedSettled). Stop -> wizStopped; errPowerCycle passes.
func manualSettle(mhz float64, gateT int) error {
	gate := float64(gateT) / 1000 * 2.4
	if !waitFor(fixedHashWait, 15*time.Second, func() bool { r := read(); return stopRequested() || (r.ok && r.mining && r.minRate > gate) }) {
		wizLive("a board is not hashing on the manual profile - recovering")
		if err := recoverBoards(0, gateT); err != nil {
			return err
		}
	}
	wizLive("manual mode on: waiting for the clocks to reach %.0f MHz and the PSU to settle", mhz)
	waitFor(fixedHashWait, 10*time.Second, func() bool { return stopRequested() || fixedSettled(mhz) })
	if stopRequested() {
		return wizStopped{}
	}
	return nil
}

// droopLoad: one load of the stage.
type droopLoad struct {
	Name    string
	W       int     // estimated wall watts at the requested voltage (the user's configured min / max)
	MHz     float64 // uniform clock
	T       int     // load key (bosminer-target equivalent)
	Drives  []float64
	Got     int    // points recorded
	Stopped string // why the remaining points were skipped ("" = all measured)
}

// droopPoint: move the trim to d (deeper = setup pace; up in voltage = at once), settle, average droopReads
// board-voltage reads. dropped = a board below dropFr of ref (the point is not recorded).
func droopPoint(d float64, ref map[string]float64) (v float64, board string, dropped string, err error) {
	if d >= trimNow() {
		if err = glide(d, wizSleep, nil); err != nil {
			return 0, "", "", err
		}
	} else {
		_ = setTrim(d)
	}
	if !wizSleep(droopSettle) {
		return 0, "", "", wizStopped{}
	}
	sum, n := 0.0, 0
	for i := 0; i < droopReads; i++ {
		r := read()
		if r.ok {
			if b := boardDropped(r.rates, ref, dropFr); b != "" || !r.mining {
				if b == "" {
					b = "every"
				}
				return 0, r.enabled, b, nil
			}
			if r.volts > 0 {
				sum, n, board = sum+r.volts, n+1, r.enabled
			}
		}
		if i < droopReads-1 && !wizSleep(droopEvery) {
			return 0, "", "", wizStopped{}
		}
	}
	if n == 0 {
		return 0, board, "", fmt.Errorf("no voltage reading at %.0f %%", d)
	}
	return sum / float64(n), board, "", nil
}

// runDroop: the stage body (the miner may be in manual or tuner mode; the trim ends at 0, manual mode stays on:
// the tuner stage follows). Returns the stage detail; an error = the stage did not finish (stop, power cycle,
// manual mode refused).
func runDroop(in wizInputs, curve wizCurve, fixT int, volt float64, restoreW int) (string, error) {
	var lo, hi float64
	if raw, err := bos("GET", "/configuration/constraints", nil); err == nil {
		lo, hi = parseFreqRange(raw)
	}
	loads := []*droopLoad{
		{Name: "min", W: in.MinW, Drives: droopDrives(curve.Points, droopLoV, droopLoLim)},
		{Name: "max", W: in.MaxW, Drives: droopDrives(curve.Points, droopHiV, droopHiLim)},
	}
	var parts []string
	for _, l := range loads {
		l.MHz, l.T = droopClock(float64(l.W), fixT, volt, lo, hi)
		_ = setTrim(0) // never a clock change with the trim on
		wizLive("droop at %d W: manual mode %.0f MHz on all chips, %.2f V", l.W, l.MHz, volt)
		wizEvent("droop", "%d W: %.0f MHz (load %d W target), drives %v", l.W, l.MHz, l.T, l.Drives)
		if err := setManualMode(l.MHz, volt, restoreW); err != nil {
			return strings.Join(parts, "; "), err
		}
		if err := manualSettle(l.MHz, l.T); err != nil {
			return strings.Join(parts, "; "), err
		}
		ref := read().rates
		for _, d := range l.Drives {
			wizLive("droop at %d W (%.0f MHz): drive %.0f %%, %d reads", l.W, l.MHz, d, droopReads)
			v, b, dropped, err := droopPoint(d, ref)
			if err != nil {
				_ = setTrim(0)
				if _, stop := err.(wizStopped); stop {
					return strings.Join(parts, "; "), err
				}
				l.Stopped = err.Error()
				break
			}
			if dropped != "" {
				l.Stopped = fmt.Sprintf("board %s below %.0f %% of its rate at %.0f %%", dropped, dropFr*100, d)
				break
			}
			if b == "" {
				b = "123"
			}
			addDrivePt(drivePt{W: l.W, T: l.T, V: v, D: d, B: b, Src: "droop", L: -1})
			l.Got++
			wizEvent("droop", "%d W: %.0f %% -> %.2f V", l.W, d, v)
		}
		_ = setTrim(0) // at once: back up in voltage at a known clock
		p := fmt.Sprintf("%d W (%.0f MHz): %d of %d points", l.W, l.MHz, l.Got, len(l.Drives))
		if l.Stopped != "" {
			p += " (" + l.Stopped + ")"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; "), nil
}
