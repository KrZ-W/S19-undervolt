package main

import (
	"math"
	"testing"
	"time"
)

// linear drive model: V(d) = v0 - k*d (monotone: more drive = lower voltage); driveAt inverts it.
type lineModel struct{ v0, k float64 }

func (m lineModel) volts(d float64) float64   { return m.v0 - m.k*d }
func (m lineModel) driveAt(v float64) float64 { return (m.v0 - v) / m.k }

// runDrive feeds a plan with "passes at or above knee volts" on the model and returns the probed drives and the
// answer drive (-1 = none). It fails the test if a probe crosses the bracket (no-cross rule).
func runDrive(t *testing.T, p *drivePlan, m lineModel, predV, knee float64) ([]float64, float64) {
	t.Helper()
	got := []float64{}
	d := p.first(m.driveAt(predV))
	maxPass, minFail := -1.0, 1e9
	for {
		if d >= minFail-1e-9 || d <= maxPass+1e-9 {
			t.Fatalf("probe %.2f %% crosses the bracket (pass %.2f, fail %.2f); probes %v", d, maxPass, minFail, got)
		}
		got = append(got, d)
		v := m.volts(d)
		pass := v >= knee-1e-9
		if pass {
			maxPass = math.Max(maxPass, d)
		} else {
			minFail = math.Min(minFail, d)
		}
		nd, done := p.next(d, v, pass)
		if done {
			if !p.HasPass {
				return got, -1
			}
			return got, p.Pass
		}
		d = nd
	}
}

func newPlan(m lineModel, step float64) *drivePlan {
	return &drivePlan{Step: step, MinD: 0, MaxD: m.driveAt(9.0), LowV: 9.0, DriveAt: m.driveAt}
}

func TestDriveStepSlopeAndClamp(t *testing.T) {
	m := lineModel{11.8, 0.07}
	if d := driveStep(m.driveAt, 10.5, 0.2); math.Abs(d-0.2/0.07) > 1e-9 {
		t.Fatalf("0.2 V at 0.07 V/%%: %.3f", d)
	}
	if d := driveStep(lineModel{11.8, 0.5}.driveAt, 10.5, 0.05); d != minDriveStep {
		t.Fatalf("steep slope clamps to the minimum: %.3f", d)
	}
	if d := driveStep(lineModel{11.8, 0.01}.driveAt, 10.5, 0.2); d != maxDriveStep {
		t.Fatalf("flat slope clamps to the maximum: %.3f", d)
	}
	flat := func(float64) float64 { return 40 }
	if d := driveEq(flat, 10.5, 0.1); math.Abs(d-0.1/fallbackVPerPct) > 1e-9 {
		t.Fatalf("no slope: fallback %.3f", d)
	}
	if d := driveEq(m.driveAt, 0, 0.1); math.Abs(d-0.1/fallbackVPerPct) > 1e-9 {
		t.Fatalf("unknown voltage: fallback %.3f", d)
	}
}

func TestDrivePlanAsymmetry(t *testing.T) {
	m := lineModel{11.8, 0.07}
	up, down := 0.2/0.07, 0.1/0.07
	p := newPlan(m, gallopStep1)
	d0 := p.first(m.driveAt(10.5))
	nd, done := p.next(d0, m.volts(d0), false) // fail -> LESS drive by the whole step (0.20 V)
	if done || math.Abs((d0-nd)-up) > 0.011 {
		t.Fatalf("after a fail: %.2f -> %.2f (want -%.2f)", d0, nd, up)
	}
	p = newPlan(m, gallopStep1)
	nd, done = p.next(d0, m.volts(d0), true) // pass -> MORE drive by half the step (0.10 V)
	if done || math.Abs((nd-d0)-down) > 0.011 {
		t.Fatalf("after a pass: %.2f -> %.2f (want +%.2f)", d0, nd, down)
	}
}

func TestDrivePlanConverges(t *testing.T) {
	for _, m := range []lineModel{{11.8, 0.07}, {11.6, 0.05}, {12.0, 0.12}} {
		for _, step := range []float64{gallopStep1, gallopStepNext} {
			for knee := 9.3; knee <= 11.3; knee += 0.037 {
				for _, off := range []float64{-0.3, -0.1, 0, 0.12, 0.3} {
					p := newPlan(m, step)
					got, ans := runDrive(t, p, m, knee+off, knee)
					if ans < 0 {
						t.Fatalf("knee %.3f pred %+.2f: no answer, probes %v", knee, off, got)
					}
					v := m.volts(ans)
					res := math.Max(minDriveStep*m.k, bisectRes) + 0.01*m.k // bracket + drive rounding
					if v < knee-1e-9 || v-knee > res+1e-9 {
						t.Fatalf("knee %.3f pred %+.2f: answer %.2f %% = %.3f V, probes %v", knee, off, ans, v, got)
					}
					if len(got) >= maxSteps {
						t.Fatalf("knee %.3f pred %+.2f: hit the step cap: %v", knee, off, got)
					}
				}
			}
		}
	}
}

func TestDrivePlanBounds(t *testing.T) {
	m := lineModel{11.8, 0.07}
	p := newPlan(m, gallopStep1)
	got, ans := runDrive(t, p, m, 9.1, 0) // everything passes: up to MaxD (the lowest allowed V), then done
	if ans != p.MaxD || got[len(got)-1] != p.MaxD {
		t.Fatalf("floor side: probes %v answer %.2f", got, ans)
	}
	p = newPlan(m, gallopStep1)
	got, ans = runDrive(t, p, m, 11.3, 99) // nothing passes: down to MinD (0 %), then no answer
	if ans >= 0 || got[len(got)-1] != 0 {
		t.Fatalf("stock side: probes %v answer %.2f", got, ans)
	}
	if d := newPlan(m, gallopStep1).first(200); d != r2(m.driveAt(9.0)) {
		t.Fatalf("first clamps to MaxD: %.2f", d)
	}
	// a step whose MEAN voltage is already at the lowest allowed voltage caps the drive there
	p = newPlan(m, gallopStep1)
	if _, done := p.next(30, 8.95, true); !done || p.MaxD != 30 {
		t.Fatalf("LowV cap: MaxD %.2f", p.MaxD)
	}
	// a monotone model that passes everywhere above 9.0 V must stop within maxSteps even with tiny steps
	p = &drivePlan{Step: 0.001, MinD: 0, MaxD: 100, LowV: 0, DriveAt: m.driveAt}
	if got, _ := runDrive(t, p, lineModel{11.8, 0.001}, 11.79, 0); len(got) != maxSteps {
		t.Fatalf("step cap: %d steps", len(got))
	}
}

func TestDrivePlanNoCross(t *testing.T) {
	m := lineModel{11.8, 0.07}
	p := newPlan(m, gallopStep1)
	p.next(40, m.volts(40), false) // fail at 40 %
	p.next(37.14, m.volts(37.14), true)
	// bracket 37.14..40: next must be strictly inside
	nd, done := p.next(38.57, m.volts(38.57), true)
	if done || nd <= 38.57 || nd >= 40 {
		t.Fatalf("inside the bracket: %.2f done %v", nd, done)
	}
	// a pass recorded at a drive that would put the next gallop past a failure: done, never at the failure
	p = newPlan(m, gallopStep1)
	p.Fail, p.HasFail = 40, true
	if nd, done := p.next(39.9, m.volts(39.9), true); !done {
		t.Fatalf("gap below resolution must end the search, got %.2f", nd)
	}
}

func TestLatchStart(t *testing.T) {
	// answer = index 1 (drive 52); drives: 0 above the answer's voltage, 2 below it
	steps := []wizStep{{Drive: 50}, {Drive: 52}, {Drive: 53.5}, {Drive: 51}}
	cases := []struct {
		since         []int
		direct, reset bool
		last          int
		why           string
	}{
		{[]int{1}, true, false, 1, "(a) the answer itself was the last step"},
		{[]int{1, 2}, true, false, 2, "(a) the failing step just below the answer was last (not reset)"},
		{[]int{2, 0}, false, true, 0, "(b) last step above the answer, a lower step ran since the reset: reset first"},
		{[]int{3}, false, false, 3, "(b) last step above the answer, nothing lower since the reset: hold only"},
		{nil, false, false, -1, "(b) a reset came after the last step: hold only"},
	}
	for _, c := range cases {
		d, r, l := latchStart(steps, c.since, 1)
		if d != c.direct || r != c.reset || l != c.last {
			t.Errorf("%s: direct %v reset %v last %d", c.why, d, r, l)
		}
	}
}

func TestLatchVerdict(t *testing.T) {
	if latchVerdict(false, true) != "cleared" || latchVerdict(false, false) != "cleared" {
		t.Error("clean from the answer or below: answer cleared")
	}
	if latchVerdict(true, false) != "latches" {
		t.Error("latched from the answer itself: the answer latches")
	}
	if latchVerdict(true, true) != "inconclusive" {
		t.Error("latched from a step below the answer: inconclusive")
	}
}

func TestMeanBoardVolts(t *testing.T) {
	rs := []reading{
		{ok: true, mining: true, bvolts: map[string]float64{"1": 10.2, "2": 10.6}},
		{ok: true, mining: true, bvolts: map[string]float64{"1": 10.6}},         // board 2 not refreshed
		{ok: true, mining: false, bvolts: map[string]float64{"1": 20, "2": 20}}, // not mining: skipped
	}
	m := meanBoardVolts(rs)
	if math.Abs(m["1"]-10.4) > 1e-9 || math.Abs(m["2"]-10.6) > 1e-9 {
		t.Fatalf("means %v", m)
	}
	if meanBoardVolts(nil) != nil {
		t.Fatal("no readings must give nil")
	}
	mx := mixVolts(map[string]float64{"1": 10.0, "2": 10.5}, map[string]float64{"1": 10.5, "3": 9.9}, 0.4, 0.6)
	if mx["1"] != 10.3 || mx["2"] != 10.5 || mx["3"] != 9.9 {
		t.Fatalf("mix %v", mx)
	}
}

func TestLatchedAtNote(t *testing.T) {
	if latchedAtNote(nil) != "" || latchedAtNote([]float64{10.3}) != "latched at 10.30 V" ||
		latchedAtNote([]float64{10.3, 10.35}) != "latched at 10.30 V and 10.35 V" ||
		latchedAtNote([]float64{10.3, 10.35, 10.4}) != "latched at 10.30 V, 10.35 V and 10.40 V" {
		t.Fatal("latchedAtNote")
	}
}

func TestFanRuleTimings(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	wiz := fanTimesWiz
	wiz.min, wiz.max, wiz.capW = 15, 25, -1
	core := fanTimesCore
	core.min, core.max, core.capW = 15, 30, 5000

	// hot: up +5, then again after lim.up (wizard 1 min, production 2 min)
	f := &fanRule{pct: 15}
	if np := f.step(true, 75, 1000, wiz, t0); np != 20 {
		t.Fatalf("wizard up: %d", np)
	}
	if np := f.step(true, 75, 1000, wiz, t0.Add(time.Minute)); np != 25 {
		t.Fatalf("wizard up after 1 min: %d", np)
	}
	if np := f.step(true, 76, 1000, wiz, t0.Add(2*time.Minute)); np != 25 {
		t.Fatalf("wizard max bound: %d", np)
	}
	f = &fanRule{pct: 15}
	f.step(true, 75, 1000, core, t0)
	if np := f.step(true, 75, 1000, core, t0.Add(time.Minute)); np != 20 {
		t.Fatalf("production keeps 2 min between moves up: %d", np)
	}

	// cool: -2 every lim.down once three readings are <= 70 and not rising
	f = &fanRule{pct: 25}
	for i, want := range []int{25, 25, 23, 21, 19} {
		if np := f.step(true, 65, 1000, wiz, t0.Add(time.Duration(i)*time.Minute)); np != want {
			t.Fatalf("wizard down minute %d: %d want %d", i, np, want)
		}
	}
	f = &fanRule{pct: 25}
	for i, want := range []int{25, 25, 23, 23, 23, 21} {
		if np := f.step(true, 65, 1000, core, t0.Add(time.Duration(i)*time.Minute)); np != want {
			t.Fatalf("production down minute %d: %d want %d", i, np, want)
		}
	}

	// a +300 W jump holds moves down for pwHold (wizard 3 min, production 10 min)
	f = &fanRule{pct: 25, lastW: 1000}
	f.step(true, 65, 1400, wiz, t0) // jump (fan above the matched value: no change)
	got := []int{}
	for i := 1; i <= 4; i++ {
		got = append(got, f.step(true, 65, 1400, wiz, t0.Add(time.Duration(i)*time.Minute)))
	}
	if got[0] != 25 || got[1] != 25 || got[2] != 23 {
		t.Fatalf("wizard power-jump hold: %v", got)
	}
	f = &fanRule{pct: 25, lastW: 1000}
	f.step(true, 65, 1400, core, t0)
	for i := 1; i < 10; i++ {
		if np := f.step(true, 65, 1400, core, t0.Add(time.Duration(i)*time.Minute)); np != 25 {
			t.Fatalf("production power-jump hold minute %d: %d", i, np)
		}
	}
	// the wizard has no cap condition; production does not step down while the cap binds
	f = &fanRule{pct: 25}
	capped := core
	capped.capW = 900
	for i := 0; i < 6; i++ {
		if np := f.step(true, 65, 1000, capped, t0.Add(time.Duration(i)*time.Minute)); np != 25 {
			t.Fatalf("cap binding: %d", np)
		}
	}
}

func TestChipRatesSources(t *testing.T) {
	chipRates := func(a, b string, m float64) (map[string][]float64, string) {
		return chipRates(parseChipMetrics(a), parseChipMetrics(b), m, nil)
	}
	a := `chip_shares{chip_addr="000",chip_in_domain="00",hashboard="1",type="valid",voltage_domain="00"} 100 1`
	b := `chip_shares{chip_addr="000",chip_in_domain="00",hashboard="1",type="valid",voltage_domain="00"} 160 1
chip_hashrate{chip_addr="000",chip_in_domain="00",hashboard="1",type="valid",voltage_domain="00"} 1.5e11 1`
	if r, src := chipRates(a, b, 2); src != "shares" || r["1"][0] != 30 {
		t.Fatalf("shares: %v %s", r["1"][:1], src)
	}
	if r, src := chipRates("", b, 2); src != "hashrate" || r["1"][0] != 1.5e11 {
		t.Fatalf("no first scrape: chip_hashrate gauge: %v %s", r, src)
	}
	stock := `chip_hashrate{chip_addr="005",chip_in_domain="01",hashboard="2",type="valid",voltage_domain="02"} 2e11 1
chip_hashrate{chip_addr="005",chip_in_domain="01",hashboard="2",type="error",voltage_domain="02"} 7 1`
	if r, src := chipRates(stock, stock, 5); src != "hashrate" || r["2"][5] != 2e11 {
		t.Fatalf("stock build (no chip_shares): %v %s", r, src)
	}
	if r, src := chipRates("", "nothing", 1); r != nil || src != "" {
		t.Fatal("no per-chip data")
	}
}
