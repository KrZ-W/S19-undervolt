package main

import (
	"math"
	"testing"
)

// realSweep: the first real 0-100 % sweep (2026-11-25, 250 MHz test profile, ≈ 962 W target, read 2 %/4 s with
// lag: stale duplicate reads at 42/44, 46/48, 56/58, 66/68).
func realSweep() wizCurve {
	c := wizCurve{Watts: 962}
	for d := 0; d <= 30; d += 2 {
		c.Points = append(c.Points, calPoint{Duty: float64(d), V: 11.80 - 0.05*float64(d)/30})
	}
	for _, p := range [][2]float64{{32, 11.62}, {34, 11.63}, {36, 11.56}, {38, 11.50}, {40, 11.45}, {42, 11.30}, {44, 11.30},
		{46, 11.04}, {48, 11.04}, {50, 11.08}, {52, 10.80}, {54, 10.65}, {56, 10.36}, {58, 10.36}, {60, 9.88}, {62, 9.75},
		{64, 9.57}, {66, 9.28}, {68, 9.28}, {70, 8.99}, {72, 8.98}, {74, 8.90}, {76, 8.84}, {78, 8.83}} {
		c.Points = append(c.Points, calPoint{Duty: p[0], V: p[1]})
	}
	for i, d := 0, 80; d <= 100; i, d = i+1, d+2 {
		c.Points = append(c.Points, calPoint{Duty: float64(d), V: 8.80 + 0.02*float64(i%2)})
	}
	return c
}

// realSteps: the same night's search steps (drive, hold-mean V, bosminer target; short = 2-min check).
func realSteps() []drivePt {
	var ps []drivePt
	for _, s := range []struct {
		d, v  float64
		w     int
		short bool
	}{{62.8, 9.56, 745, false}, {61.5, 9.81, 745, false}, {62.2, 9.69, 745, false}, {62.5, 9.67, 745, false},
		{62.7, 9.64, 745, false}, {59.9, 9.81, 970, false}, {58.6, 9.98, 970, false}, {57.8, 10.09, 970, true},
		{58.2, 10.07, 970, false}, {58.0, 10.02, 970, true}} {
		ps = append(ps, drivePt{W: s.w, V: s.v, D: s.d, B: "123", Src: "step", Short: s.short})
	}
	return ps
}

// The miss that motivated the model: level 800 W (target 970 W) aimed 9.99 V; the measured answer is 58.6 %.
// Fitted on the sweep + the 745 W steps only, the model must land within 0.5 % of it.
func TestDriveModelRealData(t *testing.T) {
	c := realSweep()
	var s745 []drivePt
	for _, p := range realSteps() {
		if p.W == 745 {
			s745 = append(s745, p)
		}
	}
	m, ok := newDriveModel(c, s745)
	if !ok {
		t.Fatal("no model")
	}
	d, _ := m.Drive(9.99, 970)
	t.Logf("745 W steps only: delta %.3f %%, k %.5f %%/W, n %d -> 9.99 V at 970 W: %.2f %% (measured 58.6)", m.Delta, m.K, m.N, d)
	if math.Abs(d-58.6) > 0.5 {
		t.Errorf("9.99 V at 970 W: %.2f %%, want 58.6 ± 0.5", d)
	}
	if s, _ := newDriveModel(c, nil); true {
		d0, _ := s.Drive(9.99, 970)
		t.Logf("sweep shape alone: %.2f %%", d0)
	}
	all, _ := newDriveModel(c, realSteps())
	da, _ := all.Drive(9.99, 970)
	t.Logf("all steps: delta %.3f %%, k %.5f %%/W -> 9.99 V at 970 W: %.2f %%", all.Delta, all.K, da)
	if all.K <= 0 {
		t.Errorf("k %.5f: a higher load should need less drive", all.K)
	}
	// startDrive goes through the same model
	if sd := startDrive([]wizCurve{c}, s745, 9.99, 970, "123"); math.Abs(sd-d) > 0.01 {
		t.Errorf("startDrive %.2f vs model %.2f", sd, d)
	}
	// monotone: more drive never gives more voltage
	for x := 0.0; x < 100; x += 0.25 {
		if all.V(x+0.25, 900) > all.V(x, 900)+1e-9 {
			t.Fatalf("not monotone at %.2f", x)
		}
	}
}

// Leave-one-out on the real steps: each step's drive predicted from the sweep + the other nine.
func TestDriveModelLeaveOneOut(t *testing.T) {
	c, st := realSweep(), realSteps()
	var sq, mx float64
	for i, p := range st {
		rest := append(append([]drivePt{}, st[:i]...), st[i+1:]...)
		m, _ := newDriveModel(c, rest)
		d, _ := m.Drive(p.V, float64(p.W))
		e := d - p.D
		sq, mx = sq+e*e, math.Max(mx, math.Abs(e))
		t.Logf("%5.1f %% %.2f V %d W: predicted %.2f (%+.2f)", p.D, p.V, p.W, d, e)
	}
	rms := math.Sqrt(sq / float64(len(st)))
	t.Logf("leave-one-out: rms %.2f %%, max %.2f %%", rms, mx)
	if rms > 0.7 || mx > 1.5 {
		t.Errorf("leave-one-out rms %.2f max %.2f", rms, mx)
	}
}

// One wild read in the sweep does not drag the shape (3-point median before the isotonic fit).
func TestDriveModelSpike(t *testing.T) {
	c := realSweep()
	m0, _ := newDriveModel(c, nil)
	for i := range c.Points {
		if c.Points[i].Duty == 50 {
			c.Points[i].V = 9.0
		}
	}
	m1, _ := newDriveModel(c, nil)
	a, _ := m0.Drive(9.99, 962)
	b, _ := m1.Drive(9.99, 962)
	if math.Abs(a-b) > 0.1 {
		t.Errorf("spike moved 9.99 V from %.2f to %.2f", a, b)
	}
}

// Few points: none = the shape alone; one = a constant offset (k stays 0).
func TestDriveModelFewPoints(t *testing.T) {
	c := realSweep()
	m, _ := newDriveModel(c, nil)
	if m.Delta != 0 || m.K != 0 || m.N != 0 {
		t.Errorf("no steps: %+v", m)
	}
	m, _ = newDriveModel(c, realSteps()[6:7])
	if m.N != 1 || m.K != 0 {
		t.Errorf("one step: n %d k %v", m.N, m.K)
	}
	if d, _ := m.Drive(9.98, 970); math.Abs(d-58.6) > 0.01 {
		t.Errorf("one step reproduces itself: %.2f", d)
	}
}
