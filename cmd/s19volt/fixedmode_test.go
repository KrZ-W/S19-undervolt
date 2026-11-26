package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixedProfileConservative(t *testing.T) {
	tg, mhz := fixedProfile()
	if mhz != 250 || tg < 900 || tg > 1000 {
		t.Fatalf("fixed profile %v MHz ~ %d W, want 250 MHz ~ 900-1000 W", mhz, tg)
	}
}

func TestModeBodies(t *testing.T) {
	b, _ := json.Marshal(manualModeBody(250, 11.96))
	want := `{"manualmode":{"global_frequency":{"hertz":250000000},"global_voltage":{"volt":11.96},"hashboards":[]}}`
	if string(b) != want {
		t.Errorf("manual body %s", b)
	}
	b, _ = json.Marshal(tunerModeBody(1300))
	want = `{"tunermode":{"target":{"powertarget":{"power_target":{"watt":1300}}}}}`
	if string(b) != want {
		t.Errorf("tuner body %s", b)
	}
}

func TestModeIsManual(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"tunermode":{"target":{"powertarget":{"power_target":{"watt":2200}}}}}`: false,
		`{"manualmode":{"global_frequency":{"hertz":250000000}}}`:                 true,
		`{"manual_mode":{}}`: true, `{"manualMode":{}}`: true, `nonsense`: false,
	} {
		if modeIsManual(json.RawMessage(raw)) != want {
			t.Errorf("%s: want %v", raw, want)
		}
	}
	if parseModeTarget(json.RawMessage(`{"manualmode":{}}`)) != 0 {
		t.Error("manual mode has no power target")
	}
}

func TestParseMinVolt(t *testing.T) {
	for raw, want := range map[string]float64{
		`{"tuner_constraints":{"power_target":{"min":{"watt":359}}},"hashboards_constraints":{"voltage":{"min":{"volt":11.96},"max":{"volt":15.5}}}}`: 11.96,
		`{"hashboardsConstraints":{"voltage":{"min":{"volt":12.1}}}}`:                                                                                 12.1,
		`{"tuner_constraints":{"power_target":{"min":{"watt":359}}}}`:                                                                                 0,
		`{"x":{"voltage":{"min":{"volt":0.5}}}}`:                                                                                                      0, // not a PSU value
	} {
		if v := parseMinVolt(json.RawMessage(raw)); v != want {
			t.Errorf("%s: %v, want %v", raw, v, want)
		}
	}
}

// A wizard.json from before the fixed profile (a "prepare" stage) still loads; the restore flag survives a reload.
func TestWizLoadPrepareStageAndRestoreFlag(t *testing.T) {
	dir := t.TempDir()
	old := ccfgPath
	ccfgPath = filepath.Join(dir, "core.json")
	wizMu.Lock()
	saved := wiz
	wizMu.Unlock()
	defer func() { ccfgPath = old; wizMu.Lock(); wiz = saved; wizMu.Unlock() }()
	js := `{"stages":[{"key":"check","status":"done"},{"key":"prepare","title":"Braiins tunes the first level (500 W)","status":"done"},
		{"key":"wiring","status":"running"}],"manual_restore_w":1300}`
	if err := os.WriteFile(filepath.Join(dir, "wizard.json"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	wizLoad()
	s := wizSnap()
	if len(s.Stages) != 3 || s.Stages[1].Key != "prepare" || s.Stages[2].Status != "failed" || manualRestoreW() != 1300 || s.Fixed != nil {
		t.Fatalf("old state: %+v", s)
	}
}

func TestClocksReached(t *testing.T) {
	if clocksReached([]float64{68, 68, 70}, 250) || !clocksReached([]float64{250, 249, 250, 120}, 250) || clocksReached(nil, 250) {
		t.Fatal("clocksReached")
	}
	if clocksReached([]float64{455, 460, 470}, 250) { // still on the previous tune (2026-11-25 run 7: passed in 1 s)
		t.Fatal("clocksReached: faster old tune must not count")
	}
}

func TestSteady(t *testing.T) {
	m := func(a, b float64) map[string]float64 { return map[string]float64{"1": a, "2": b} }
	if steady([]map[string]float64{m(10, 10), m(10.1, 9.9)}, 3, 0.03) {
		t.Fatal("too few readings")
	}
	if !steady([]map[string]float64{m(5, 5), m(10, 10), m(10.2, 9.9), m(9.9, 10.1)}, 3, 0.03) {
		t.Fatal("should be steady")
	}
	if steady([]map[string]float64{m(8, 10), m(9, 10), m(10, 10)}, 3, 0.03) {
		t.Fatal("board 1 still climbing")
	}
}
