package main

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

// Clock per load: power ∝ clock at fixed voltage, scaled from the test profile (250 MHz ≈ target 962 W); clamped
// to the sane bounds and to Braiins' constraints when given.
func TestDroopClock(t *testing.T) {
	fixT, _ := fixedProfile()
	for _, c := range []struct {
		w, lo, hi, want float64
	}{
		{600, 0, 0, 135}, {2000, 0, 0, 496},
		{estWall(fixT, 11.96), 0, 0, fixedMHz},            // the test profile's own load gives its own clock
		{150, 0, 0, droopMHzLo}, {5000, 0, 0, droopMHzHi}, // sane bounds
		{2000, 0, 400, 400}, {600, 200, 0, 200}, // Braiins' constraints
		{2000, 0, 900, 496}, // a constraint wider than the sane bounds does not widen them
	} {
		mhz, load := droopClock(c.w, fixT, 11.96, c.lo, c.hi)
		if mhz != c.want {
			t.Errorf("%.0f W [%v..%v]: %.0f MHz, want %.0f", c.w, c.lo, c.hi, mhz, c.want)
		}
		if want := int(math.Round(float64(fixT) * mhz / fixedMHz)); load != want {
			t.Errorf("%.0f W: load %d, want %d", c.w, load, want)
		}
		t.Logf("%.0f W -> %.0f MHz, load %d", c.w, mhz, load)
	}
}

func TestParseFreqRange(t *testing.T) {
	for raw, want := range map[string][2]float64{
		`{"hashboards_constraints":{"frequency":{"min":{"hertz":50000000},"max":{"hertz":800000000}}}}`: {50, 800},
		`{"hashboardsConstraints":{"frequencyMhz":{"min":{"mhz":100}}}}`:                                {100, 0},
		`{"tuner_constraints":{"power_target":{"min":{"watt":359}}}}`:                                   {0, 0},
		`{"frequency":{"min":{"hertz":5}}}`:                                                             {0, 0}, // implausible
	} {
		lo, hi := parseFreqRange(json.RawMessage(raw))
		if lo != want[0] || hi != want[1] {
			t.Errorf("%s: %v..%v, want %v", raw, lo, hi, want)
		}
	}
}

// Drives from tonight's sweep: nearest measured points, never past the 9.6 V (low load) / 11.3 V (high load) point.
func TestDroopDrives(t *testing.T) {
	ps := realSweep().Points
	lo := droopDrives(ps, droopLoV, droopLoLim)
	hi := droopDrives(ps, droopHiV, droopHiLim)
	t.Logf("low load %v, high load %v", lo, hi)
	if !reflect.DeepEqual(lo, []float64{52, 56, 62}) {
		t.Errorf("low load drives %v", lo)
	}
	if !reflect.DeepEqual(hi, []float64{34, 38, 42}) {
		t.Errorf("high load drives %v", hi)
	}
	at := map[float64]float64{}
	for _, p := range ps {
		at[p.Duty] = p.V
	}
	for _, d := range lo {
		if at[d] < droopLoLim {
			t.Errorf("low load drive %v reads %.2f V < %.1f", d, at[d], droopLoLim)
		}
	}
	for _, d := range hi {
		if at[d] < droopHiLim {
			t.Errorf("high load drive %v reads %.2f V < %.1f", d, at[d], droopHiLim)
		}
	}
	// targets below the limit clamp to the last point above it; a noisy dip ends the candidates there
	if d := droopDrives(ps, []float64{8.9}, droopLoLim); !reflect.DeepEqual(d, []float64{62}) {
		t.Errorf("below the limit: %v", d)
	}
	noisy := []calPoint{{Duty: 30, V: 11.7}, {Duty: 32, V: 11.2}, {Duty: 34, V: 11.6}, {Duty: 36, V: 11.4}}
	if d := droopDrives(noisy, droopHiV, droopHiLim); !reflect.DeepEqual(d, []float64{30}) {
		t.Errorf("noisy dip: %v", d)
	}
	if d := droopDrives(nil, droopHiV, droopHiLim); len(d) != 0 {
		t.Errorf("no sweep: %v", d)
	}
}

// The fit with the sweep + 6 droop points only (consistent with tonight's two levels: generated from the model
// fitted on every real step) predicts the 970 W level's 9.99 V within 0.5 % of the measured 58.6 %.
func TestDroopFit(t *testing.T) {
	c := realSweep()
	all, _ := newDriveModel(c, realSteps())
	fixT, _ := fixedProfile()
	var pts []drivePt
	for _, l := range []struct {
		w      float64
		drives []float64
	}{{600, droopDrives(c.Points, droopLoV, droopLoLim)}, {2000, droopDrives(c.Points, droopHiV, droopHiLim)}} {
		_, load := droopClock(l.w, fixT, 11.96, 0, 0)
		for _, d := range l.drives {
			pts = append(pts, drivePt{W: int(l.w), T: load, D: d, V: math.Round(all.V(d, float64(load))*1000) / 1000, B: "123", Src: "droop", L: -1})
		}
	}
	m, ok := newDriveModel(c, pts)
	if !ok || m.N != 6 || m.ND != 6 {
		t.Fatalf("droop points in the fit: ok %v n %d nd %d (%+v)", ok, m.N, m.ND, pts)
	}
	d, _ := m.Drive(9.99, 970)
	t.Logf("droop only: delta %.3f %%, k %.5f %%/W -> 9.99 V at 970 W: %.2f %% (measured 58.6; all steps %.3f/%.5f)", m.Delta, m.K, d, all.Delta, all.K)
	if math.Abs(d-58.6) > 0.5 {
		t.Errorf("9.99 V at 970 W: %.2f %%, want 58.6 ± 0.5", d)
	}
	// startDrive (through modelFor) uses them too, also for a board set without steps of its own
	if sd := startDrive([]wizCurve{c}, pts, 9.99, 970, "12"); math.Abs(sd-d) > 0.01 {
		t.Errorf("startDrive %.2f vs model %.2f", sd, d)
	}
	// with steps of the level's board set, droop points stay in the fit
	mf, _ := modelFor([]wizCurve{c}, append(append([]drivePt{}, pts...), realSteps()...), 970, "123")
	if mf.N != 16 || mf.ND != 6 {
		t.Errorf("modelFor: n %d nd %d, want 16/6", mf.N, mf.ND)
	}
}

// Resume: a reusable preparation without droop points runs only the droop stage (+ tuner); with them nothing;
// redo or a changed miner runs everything; a miner that refused manual mode never runs it.
func TestResumePlanDroop(t *testing.T) {
	s := stoppedRun()
	s.Curves = []wizCurve{realSweep()}
	if p, _ := resumePlan(s, testMiner, false); p != "droop" {
		t.Errorf("no droop points: %s", p)
	}
	if p, _ := resumePlan(s, testMiner, true); p != "full" {
		t.Errorf("redo: %s", p)
	}
	other := testMiner
	other.Boards = 2
	if p, _ := resumePlan(s, other, false); p != "full" {
		t.Errorf("changed miner: %s", p)
	}
	fb := s
	fb.Fixed = &wizFixed{Fallback: "refused"}
	if p, _ := resumePlan(fb, testMiner, false); p != "none" {
		t.Errorf("manual mode refused: %s", p)
	}
	s.Points = append(s.Points, drivePt{Src: "droop", W: 1000, T: 900, D: 52, V: 10.8})
	if p, _ := resumePlan(s, testMiner, false); p != "none" {
		t.Errorf("droop points saved: %s", p)
	}
	// an old run gets the droop stage (before the tuner); the droop-only plan sets droop + tuner waiting
	o := stoppedRun()
	o.Curves = []wizCurve{realSweep()}
	prepareResume(&o, 1)
	var keys []string
	for _, st := range o.Stages {
		keys = append(keys, st.Key)
	}
	if !reflect.DeepEqual(keys, []string{"check", "fixed", "sweep", "droop", "tuner", "search"}) {
		t.Fatalf("stages %v", keys)
	}
	droopOnlyStages(&o)
	for _, st := range o.Stages {
		want := map[string]string{"check": "waiting", "fixed": "done", "sweep": "done", "droop": "waiting", "tuner": "waiting", "search": "waiting"}[st.Key]
		if st.Status != want {
			t.Errorf("%s: %s, want %s", st.Key, st.Status, want)
		}
	}
	// redo: droop points are dropped with the rest of the preparation, the droop stage waits again
	r := stoppedRun()
	r.Points = append(r.Points, drivePt{Src: "droop"})
	prepareResume(&r, 1)
	r.Stages[3].Status = "done"
	resetPrep(&r)
	for _, p := range r.Points {
		if p.Src == "droop" {
			t.Error("redo kept a droop point")
		}
	}
	if stageStatus(r, "droop") != "waiting" {
		t.Errorf("droop stage after redo: %s", stageStatus(r, "droop"))
	}
}
