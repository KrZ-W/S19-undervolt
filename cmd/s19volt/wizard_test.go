package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestPowerModel(t *testing.T) {
	// reference points from the metered miner (trimmed, 3 boards): target, board V, wall W
	for _, c := range []struct {
		t    int
		v, w float64
	}{{500, 9.60, 420}, {800, 9.72, 640}, {1000, 10.10, 834}, {1300, 10.61, 1142}, {2000, 11.54, 1969}} {
		if e := estWall(c.t, c.v); math.Abs(e-c.w) > 25 {
			t.Errorf("estWall(%d, %.2f) = %.0f, metered %.0f", c.t, c.v, e, c.w)
		}
		if back := targetFor(estWall(c.t, c.v), c.v); abs(back-c.t) > 5 {
			t.Errorf("targetFor inverse: %d -> %d", c.t, back)
		}
	}
}

func TestJobsOrderAndFewerBoards(t *testing.T) {
	in := wizInputs{MinW: 500, MaxW: 800, StepW: 100}
	j := in.jobs()
	if len(j) != 4 || j[0] != (wizJob{500, "123"}) || j[3].W != 800 {
		t.Fatalf("plain jobs: %v", j)
	}
	in.FewerBoards = true
	j = in.jobs()
	if j[0] != (wizJob{500, "123"}) {
		t.Fatalf("first job must rank the boards: %v", j)
	}
	for i := 2; i < len(j); i++ {
		if j[i].W < j[i-1].W {
			t.Fatalf("not low to high after the ranking job: %v", j)
		}
	}
	has := func(w int, b string) bool {
		for _, x := range j {
			if x == (wizJob{w, b}) {
				return true
			}
		}
		return false
	}
	if !has(300, "n1") || !has(300, "n2") || !has(400, "n2") || has(400, "n1") || !has(500, "n2") || has(600, "n2") {
		t.Fatalf("fewer-board jobs wrong: %v", j)
	}
}

func TestRankBoards(t *testing.T) {
	l := wizLevel{StockV: 11.8, Steps: []wizStep{
		{V: 11.5, Shares: map[string]float64{"1": 1, "2": 0.99, "3": 1}},
		{V: 11.4, Shares: map[string]float64{"1": 0.99, "2": 0.90, "3": 1}},
		{V: 11.3, Shares: map[string]float64{"1": 0.95, "2": 0.80, "3": 0.99}},
	}}
	if r := rankBoards(l); len(r) != 3 || r[0] != "3" || r[1] != "1" || r[2] != "2" {
		t.Fatalf("ranking %v, want [3 1 2]", r)
	}
}

// fast curve helper: drive 30..75 % maps linearly onto v0..v1
func lineCurve(w int, v0, v1 float64) wizCurve {
	c := wizCurve{Watts: w}
	for d := 30.0; d <= 75; d++ {
		c.Points = append(c.Points, calPoint{Duty: d, V: v0 + (d-30)/45*(v1-v0)})
	}
	return c
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.05 }

func TestSeriesDrive(t *testing.T) {
	ps := []drivePt{{V: 10, D: 60}, {V: 11, D: 40}}
	if d, in := seriesDrive(ps, 10.5); !in || !near(d, 50) {
		t.Errorf("inside: %.2f %v", d, in)
	}
	if d, in := seriesDrive(ps, 9.5); in || !near(d, 70) {
		t.Errorf("below: %.2f %v", d, in)
	}
	if d, in := seriesDrive(ps, 11.5); in || !near(d, 30) {
		t.Errorf("above: %.2f %v", d, in)
	}
	if d, in := seriesDrive(ps[:1], 9); in || d != 60 {
		t.Errorf("one point: %.2f %v", d, in)
	}
}

func TestStartDrive(t *testing.T) {
	// nothing known: the fixed start
	if d := startDrive(nil, nil, 10, 500, "123"); d != startDriveNone {
		t.Errorf("nothing known: %.2f", d)
	}
	// wiring/floor points only: interpolated along them
	wf := []drivePt{{W: 500, V: 12, D: 0, B: "123", Src: "wiring"}, {W: 500, V: 10.5, D: 50, B: "123", Src: "wiring"},
		{W: 500, V: 8.8, D: 100, B: "123", Src: "floor"}}
	if d := startDrive(nil, wf, 11.25, 500, "123"); !near(d, 25) {
		t.Errorf("wiring points: %.2f", d)
	}
	// a curve beats wiring/floor points: the fitted model (linear sweep 12 -> 9 V over 30..75 %: 10.5 V at 52.5 %)
	cs := []wizCurve{lineCurve(500, 12, 9)}
	if d := startDrive(cs, wf, 10.5, 500, "123"); !near(d, 52.5) {
		t.Errorf("curve at 500 W: %.2f", d)
	}
	// measured steps shift the curve: both 3 % less drive than the sweep (one load: constant offset)
	st := []drivePt{{W: 1000, V: 10.4, D: 51, B: "123", Src: "step"}, {W: 1000, V: 10.0, D: 57, B: "123", Src: "step"}}
	pts := append(append([]drivePt{}, wf...), st...)
	if d := startDrive(cs, pts, 9.8, 1000, "123"); !near(d, 60) {
		t.Errorf("shifted by the steps: %.2f", d)
	}
	// same board set preferred: another set's steps do not move the fit
	two := append(append([]drivePt{}, pts...), drivePt{W: 1000, V: 10.2, D: 90, B: "12", Src: "step"})
	if d := startDrive(cs, two, 9.8, 1000, "123"); !near(d, 60) {
		t.Errorf("board preference: %.2f", d)
	}
	// no curves, steps only: their own end segment extends the range; clamped to 0..100
	if d := startDrive(nil, st, 9.0, 1000, "123"); !near(d, 72) {
		t.Errorf("extrapolated: %.2f", d)
	}
	if d := startDrive(nil, st, 5, 1000, "123"); d != 100 {
		t.Errorf("clamp: %.2f", d)
	}
}

func TestMidW(t *testing.T) {
	if m := midW(wizInputs{MinW: 500, MaxW: 2200, StepW: 100}); m != 1300 {
		t.Errorf("midW = %d", m)
	}
}

// sweepPts: a 0-100 % sweep: flat (with reading noise) up to 30 %, then -0.05 V per %, flat at the floor.
func sweepPts(stop int) []calPoint {
	var p []calPoint
	for d := 0; d <= stop; d++ {
		v := 12.0
		if d%2 == 1 && d < 30 {
			v = 11.98 // noise in the flat start
		}
		if d > 30 {
			v = math.Max(8.8, 12-0.05*float64(d-30))
		}
		p = append(p, calPoint{Duty: float64(d), V: v})
	}
	return p
}

func TestActingDuty(t *testing.T) {
	if a := actingDuty(sweepPts(100), actingDV); a != 31 {
		t.Errorf("acting at %.0f, want 31", a)
	}
	// one noisy low read does not count
	p := sweepPts(100)
	p[10].V = 11.9
	if a := actingDuty(p, actingDV); a != 31 {
		t.Errorf("single dip: acting at %.0f", a)
	}
	if a := actingDuty(sweepPts(20), actingDV); a != -1 {
		t.Errorf("flat sweep: %.0f", a)
	}
	if a := actingDuty(nil, actingDV); a != -1 {
		t.Errorf("empty: %.0f", a)
	}
}

func TestSweepDetail(t *testing.T) {
	d := sweepDetail(calRun{Points: sweepPts(100), Status: "done"})
	if !strings.Contains(d, "floor 8.80 V at 100 %") || !strings.Contains(d, "starts dropping at 31 %") || strings.Contains(d, "ended") {
		t.Errorf("done: %s", d)
	}
	d = sweepDetail(calRun{Points: sweepPts(80), Status: "hashing limit at 80.00 % (9.50 V): board 2 below 60 % of its rate"})
	if !strings.Contains(d, "floor 9.50 V at 80 %") || !strings.Contains(d, "sweep ended at 80 %") || strings.Contains(d, "HIGH") {
		t.Errorf("early end: %s", d)
	}
	if !strings.Contains(sweepDetail(calRun{Points: sweepPts(100), Status: "done"}), "every board hashed to the end") {
		t.Error("no hashing note")
	}
	// boards stop hashing at 70 %: information only, the floor is still the 100 % point
	p := sweepPts(100)
	for i := 70; i <= 100; i++ {
		p[i].NotHashing = []string{"2"}
	}
	p[90].NotHashing = []string{"1", "2"}
	d = sweepDetail(calRun{Points: p, Status: "done"})
	if !strings.Contains(d, "boards stop hashing at 70 % (10.00 V) at the test profile (board 2") || !strings.Contains(d, "floor 8.80 V at 100 %") {
		t.Errorf("not hashing: %s", d)
	}
}

func TestModCheck(t *testing.T) {
	cases := []struct {
		stock, v, d float64
		want        modResult
	}{
		{12.0, 12.0, 0, modUnknown},
		{12.0, 11.9, 40, modUnknown},
		{12.0, 11.85, 42, modWorks},  // exactly 0.15 V
		{12.0, 11.86, 60, modFailed}, // 60 % reached without the drop
		{12.0, 11.80, 60, modWorks},  // the drop at 60 % still counts
		{12.0, 0, 30, modUnknown},    // no reading
		{0, 11.0, 30, modUnknown},    // no stock reading
	}
	for _, c := range cases {
		if got := modCheck(c.stock, c.v, c.d, modDV, modBy); got != c.want {
			t.Errorf("modCheck(%v, %v, %v) = %v, want %v", c.stock, c.v, c.d, got, c.want)
		}
	}
	if s := modText(11.96, 11.79, 34); s != "✓ the mod works: 11.96 → 11.79 V at 34 %" {
		t.Error(s)
	}
}

func TestNotHashing(t *testing.T) {
	ref := map[string]float64{"1": 30, "2": 30, "3": 30}
	if b := notHashing(map[string]float64{"1": 29, "2": 10, "3": 0}, ref, 0.6, true); strings.Join(b, ",") != "2,3" {
		t.Errorf("got %v", b)
	}
	if b := notHashing(map[string]float64{"1": 29, "2": 29, "3": 29}, ref, 0.6, true); len(b) != 0 {
		t.Errorf("all hashing: %v", b)
	}
	if b := notHashing(map[string]float64{"1": 29}, ref, 0.6, false); len(b) != 3 {
		t.Errorf("not mining: %v", b)
	}
	if b := notHashing(nil, ref, 0, true); b != nil {
		t.Errorf("fr 0: %v", b)
	}
}

// The 0-100 % sweep's flat start (noisy) and flat floor: the model takes the first drive that reaches v.
func TestDriveModelFlatEnds(t *testing.T) {
	c := wizCurve{Watts: 900, Points: sweepPts(100)}
	m, ok := newDriveModel(c, nil)
	if !ok {
		t.Fatal("no model")
	}
	if d, ok := m.Drive(11.5, 900); !ok || !near(d, 40) {
		t.Errorf("mid: %.2f %v", d, ok)
	}
	if d, ok := m.Drive(11.99, 900); !ok || d > 31 {
		t.Errorf("noisy start: %.2f %v", d, ok)
	}
	if d, ok := m.Drive(8.8, 900); !ok || d > 95 {
		t.Errorf("floor: %.2f %v", d, ok)
	}
	if d, ok := m.Drive(8.5, 900); ok || d != 100 {
		t.Errorf("below the floor: %.2f %v", d, ok)
	}
	if d := startDrive([]wizCurve{c}, nil, 10.5, 900, "123"); !near(d, 60) {
		t.Errorf("startDrive on the sweep: %.2f", d)
	}
}

func baseSample(t0 time.Time, sec int, th float64) rateSample {
	return rateSample{at: t0.Add(time.Duration(sec) * time.Second),
		m: chipMetrics{ok: true, shares: map[string][]float64{"1": {float64(sec) * 2}}, freqs: map[string][]float64{"1": {500}}},
		r: reading{ok: true, mining: true, rates: map[string]float64{"1": th}, chip: 60 + th/100, volts: 12}}
}

func TestBaseWindowSliding(t *testing.T) {
	hold := 5 * time.Minute
	t0 := time.Unix(1e9, 0)
	var w baseWindow
	w.restart(t0)
	for sec := 0; sec < 290; sec += 15 { // 0 .. 285 s: under 5 min
		w.add(baseSample(t0, sec, 10), hold)
	}
	if _, ok := w.last(hold); ok {
		t.Fatal("ready before 5 clean minutes")
	}
	if c := w.collected(hold); c != 285*time.Second {
		t.Fatalf("collected %v", c)
	}
	for sec := 300; sec <= 900; sec += 15 { // keep sampling well past 5 min; the buffer stays bounded
		w.add(baseSample(t0, sec, float64(sec)), hold)
	}
	if len(w.s) != 21 {
		t.Fatalf("buffer %d samples, want 21 (anchor + 20)", len(w.s))
	}
	mw, ok := w.last(hold)
	if !ok || len(mw.rs) != 20 || mw.m0.shares["1"][0] != 1200 || mw.m1.shares["1"][0] != 1800 || mw.minutes != 5 {
		t.Fatalf("last: ok %v n %d m0 %v m1 %v min %v", ok, len(mw.rs), mw.m0.shares, mw.m1.shares, mw.minutes)
	}
	rates, tot, chip, v, err := windowRates(mw.rs)
	if err != nil || rates["1"] != 757.5 || tot != 757.5 || chip != 69 || v != 12 { // mean of 615..900
		t.Fatalf("windowRates %v %v %v %v %v", rates, tot, chip, v, err)
	}
	// a voltage change discards everything: 5 clean minutes needed again
	w.restart(t0.Add(905 * time.Second))
	w.add(baseSample(t0, 915, 1), hold)
	if _, ok := w.last(hold); ok || w.collected(hold) != 0 {
		t.Fatal("restart did not discard the buffer")
	}
}

func TestWindowRatesSkipsIdle(t *testing.T) {
	rs := []reading{{ok: true, mining: false, rates: map[string]float64{"1": 99}},
		{ok: true, mining: true, rates: map[string]float64{"1": 20, "2": 30}, chip: 70, volts: 11.8}}
	rates, tot, chip, v, err := windowRates(rs)
	if err != nil || rates["1"] != 20 || tot != 50 || chip != 70 || v != 11.8 {
		t.Fatalf("%v %v %v %v %v", rates, tot, chip, v, err)
	}
	if _, _, _, _, err := windowRates(rs[:1]); err == nil {
		t.Fatal("no mining readings must be an error")
	}
}

func TestStage4Since(t *testing.T) {
	t0 := time.Unix(1e9, 0)
	var k tuneTracker
	if k.stage4Since(1000, t0) {
		t.Fatal("nothing logged")
	}
	k.stage, k.stageAt = 4, t0.Add(-time.Second)
	if k.stage4Since(1000, t0) {
		t.Fatal("stage 4 before the change counts")
	}
	k.stageAt = t0.Add(time.Second)
	if !k.stage4Since(1000, t0) {
		t.Fatal("stage line after the change")
	}
	k = tuneTracker{roundTarget: 900, lastRound: t0.Add(time.Minute)}
	if k.stage4Since(1000, t0) {
		t.Fatal("round for another target")
	}
	k.roundTarget = 1000
	if !k.stage4Since(1000, t0) {
		t.Fatal("round for the target")
	}
}

// the buffer holds parsed samples only: trimmed to the anchor + newer, discarded on restart, and the chip work of the
// last 5 min comes from the parsed share counters (same result as two raw scrapes 5 min apart).
func TestBaseWindowParsedChips(t *testing.T) {
	hold := 5 * time.Minute
	t0 := time.Unix(1e9, 0)
	var w baseWindow
	w.restart(t0)
	for sec := 0; sec <= 600; sec += 15 {
		w.add(baseSample(t0, sec, 10), hold)
	}
	if w.s[0].at != t0.Add(300*time.Second) || len(w.s) != 21 {
		t.Fatalf("trim: first %v, %d samples", w.s[0].at.Sub(t0), len(w.s))
	}
	mw, ok := w.last(hold)
	if !ok {
		t.Fatal("not ready")
	}
	rates, _, _, _, _ := windowRates(mw.rs)
	windowChips(mw, rates)
	if lastChipSrc != "shares" || lastChips["1"][0] != 120 || lastFreqs["1"][0] != 500 { // (1200-600)/5 min
		t.Fatalf("chips %v src %q freqs %v", lastChips, lastChipSrc, lastFreqs)
	}
	mw.m1 = chipMetrics{}
	windowChips(mw, rates) // failed last scrape: no per-chip data
	if lastChips != nil || lastChipSrc != "" {
		t.Fatal("failed scrape kept chip data")
	}
	w.restart(t0.Add(601 * time.Second))
	if len(w.s) != 0 {
		t.Fatal("restart kept samples")
	}
	if _, ok := w.last(hold); ok {
		t.Fatal("ready after a discard")
	}
}

func TestSweepComplete(t *testing.T) {
	if sweepComplete(4, false, "stopped at 4.00 %: no voltage reading") == nil {
		t.Fatal("an early end must fail")
	}
	if sweepComplete(100, false, "done") == nil {
		t.Fatal("no voltage drop must fail")
	}
	if err := sweepComplete(100, true, "done"); err != nil {
		t.Fatal(err)
	}
}

func TestFanSetCheckNudges(t *testing.T) {
	var got []int
	cur := 25
	set := func(p int) (string, error) { // fails when asked for the PWM it already has (no movement to confirm)
		got = append(got, p)
		if p == cur {
			return "", fmt.Errorf("no candidate moved the PWM")
		}
		cur = p
		return "ok", nil
	}
	if _, err := fanSetCheck(25, set, 0); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 23 || got[1] != 25 {
		t.Fatalf("calls %v, want [23 25]", got)
	}
}

func TestCaughtUp(t *testing.T) {
	pre := map[string]float64{"1": 14.8, "2": 14.5, "3": 14.9}
	if caughtUp(map[string]float64{"1": 3.6, "2": 14.5, "3": 14.9}, pre, 0.9) {
		t.Fatal("a ramping board (3.6 of 14.8) is not caught up")
	}
	if !caughtUp(map[string]float64{"1": 13.5, "2": 13.2, "3": 14.0}, pre, 0.9) {
		t.Fatal("all boards >= 90 % are caught up")
	}
	if caughtUp(nil, pre, 0.9) {
		t.Fatal("no reading is not caught up")
	}
}

func TestFanDrifted(t *testing.T) {
	if !fanDrifted(25, 15) || fanDrifted(16, 15) || fanDrifted(13, 15) || !fanDrifted(12, 15) {
		t.Fatal("fanDrifted")
	}
}
