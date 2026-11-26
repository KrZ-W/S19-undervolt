package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	lnS3    = "2026-11-25T10:00:00.000000Z INFO bosminer_plus_tuner::tune: Tune/all: S3 it33: target 1000.00 W, measured n/a, estimated 828.58 W, 11.960 V | hb1 261.2 MHz mcr 99.9% | hb2 261.2 MHz mcr 99.0% | hb3 258.4 MHz mcr 98.2%"
	lnStage = "2026-11-25T10:00:00.000000Z INFO bosminer_plus_tuner::tune: Tune/all: Using strategy s3: Entering STAGE 2: Finding threshold voltage"
)

func TestParseRound(t *testing.T) {
	r, ok := parseRound(lnS3)
	if !ok || r.Stage != 3 || r.It != 33 || r.Target != 1000 || r.EstW != 828.58 || r.V != 11.96 || r.Measured != "n/a" {
		t.Fatalf("S3 round: %+v %v", r, ok)
	}
	if len(r.Boards) != 3 || r.Boards[0] != (tunerBoard{"1", 261.2, 99.9}) || r.Boards[2].MCR != 98.2 {
		t.Fatalf("boards: %+v", r.Boards)
	}
	r, ok = parseRound(lnRound)
	if !ok || r.Stage != 4 || r.It != 3 || r.Target != 2000 || r.V != 12.021 || len(r.Boards) != 1 || r.Boards[0].MHz != 539 {
		t.Fatalf("S4 round: %+v", r)
	}
	if _, ok := parseRound(lnRamp); ok {
		t.Fatal("a ramp line is not a round")
	}
	if k, _ := classifyLine(lnS3); k != lineOther {
		t.Fatal("a stage-3 round is the tune itself, not a stage-4 round")
	}
}

func TestParseStage(t *testing.T) {
	for _, c := range []struct {
		line string
		n    int
		name string
	}{
		{lnStage, 2, "Finding threshold voltage"},
		{"INFO Tune/all: Entering STAGE 7: measuring baseline performance", 7, "measuring baseline performance"},
		{"INFO bosminer_plus_tuner::tune: Entering STAGE 4: stable", 4, "stable"},
		{"INFO Entering STAGE 3", 3, ""},
	} {
		n, name, ok := parseStage(c.line)
		if !ok || n != c.n || name != c.name {
			t.Errorf("parseStage(%q) = %d %q %v", c.line, n, name, ok)
		}
	}
}

func TestTunerLine(t *testing.T) {
	var k tuneTracker
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	if ev := k.feed(lnStage, now.Add(-20*time.Minute)); ev != "Braiins: stage 2 (Finding threshold voltage)" {
		t.Fatalf("stage event %q", ev)
	}
	k.feed("INFO Tune/all: Entering STAGE 3: tuning", now.Add(-10*time.Minute))
	if ev := k.feed(lnS3, now.Add(-time.Minute)); ev != "" {
		t.Fatalf("a round is no event: %q", ev)
	}
	k.wait, k.waitW, k.waitAt = "stage 4", 1000, now.Add(-30*time.Second)
	want := "Braiins tuning 1000 W from scratch: stage 3 round 33 (est. 829 W, 11.96 V, mcr 98–100 %) — waiting for stage 4"
	if got := k.view(now).Line; got != want {
		t.Fatalf("line\n got %q\nwant %q", got, want)
	}
	// a stage entered after the last round is shown first
	k.feed("INFO Tune/all: Entering STAGE 7: measuring baseline performance", now)
	if got := k.view(now).Line; !strings.HasPrefix(got, "Braiins tuning 1000 W from scratch: stage 7 (measuring baseline performance), after stage 3 round 33") {
		t.Fatalf("stage after round: %q", got)
	}
	// stage 4 and its rounds
	k.feed("INFO Entering STAGE 4: stable", now)
	k.feed(lnRound, now.Add(time.Second))
	k.wait, k.waitW = "", 0
	v := k.view(now.Add(2 * time.Second))
	if !strings.HasPrefix(v.Line, "Braiins at 2000 W: stage 4 round 3 (est. 1936 W, 12.02 V, mcr 100 %)") || v.Round.Target != 2000 || v.Stage != 4 {
		t.Fatalf("stage 4: %+v", v)
	}
	// a wait for another target: the old round is not shown as if it were this one
	k.wait, k.waitW, k.waitAt = "the power target to be accepted", 1100, now
	if got := k.view(now.Add(2 * time.Minute)).Line; got != "Braiins at 1100 W: stage 4 (stable), no tuning round yet — waiting for the power target to be accepted (2 min so far)" {
		t.Fatalf("other target: %q", got)
	}
	// stale lines say so
	k.wait = ""
	if got := k.view(now.Add(20 * time.Minute)).Line; !strings.Contains(got, "last tuner line 20 min ago") {
		t.Fatalf("stale: %q", got)
	}
	// a restart clears the tuner state
	if ev := k.feed("INFO Starting BOSminer 26.11", now.Add(21*time.Minute)); ev != "bosminer started" {
		t.Fatalf("restart event %q", ev)
	}
	if got := k.view(now.Add(22 * time.Minute)).Line; got != "bosminer started 60 s ago, Braiins not tuning yet" {
		t.Fatalf("restart: %q", got)
	}
}

func TestWizEvents(t *testing.T) {
	wizMu.Lock()
	saved := wiz
	wiz = wizState{}
	wizMu.Unlock()
	defer func() { wizMu.Lock(); wiz = saved; wizMu.Unlock() }()
	wizEvent("note", "not running")
	if len(wizSnap().Events) != 0 {
		t.Fatal("events only while the wizard runs")
	}
	wizMu.Lock()
	wiz.Running = true
	wizMu.Unlock()
	for i := 0; i < 45; i++ {
		wizEvent("note", "e%d", i)
	}
	ev := wizSnap().Events
	if len(ev) != maxWizEvents || ev[0].Text != "e15" || ev[len(ev)-1].Text != "e44" || ev[0].T == 0 {
		t.Fatalf("kept %d, first %+v", len(ev), ev[0])
	}
}

func TestEventTexts(t *testing.T) {
	if s := glideText(0, 50, 11.82, 11.21, nil); s != "trim 0 → 50 %, 11.82 → 11.21 V" {
		t.Error(s)
	}
	if s := glideText(50, 30, 11.2, 11.5, errAborted); !strings.Contains(s, "ended early") {
		t.Error(s)
	}
	st := wizStep{V: 10.4, Drive: 61.5, THs: 50.12, JTH: 21.3, Share: 0.985, Chip: 68, NewWeak: 1, Pass: true, Full: true, Fan: 22, MCR: 0.995}
	if s := holdText(st); s != "drive 61.5 % -> 10.40 V avg, 5-min hold: 50.12 TH/s, est. 21.30 J/TH, slowest board 98 %, chip 68 °C, fan 22 %, 1 new weak chips, step mcr 99.5 %, pass" {
		t.Error(s)
	}
	if s := holdText(wizStep{V: 9.9, Drive: 70, Dropped: "board 2 at 40 %"}); !strings.Contains(s, "aborted - board 2") {
		t.Error(s)
	}
	if s := latchText(true, []string{"2:14"}, true); s != "1 chips latched (2:14), a board down" {
		t.Error(s)
	}
	if s := latchText(false, nil, false); s != "no per-chip data" {
		t.Error(s)
	}
	if s := levelResult(wizLevel{Status: "done", V: 10.45, Answer: "efficiency", EstW: 1002, EstJTH: 21.04}, nil); s != "done at 10.45 V (efficiency), est. 1002 W, 21.0 J/TH" {
		t.Error(s)
	}
	if s := levelResult(wizLevel{}, errors.New("x")); s != "failed: x" {
		t.Error(s)
	}
}

func TestEstimateFixedProfile(t *testing.T) {
	e := estMinutes(wizInputs{MinW: 500, MaxW: 800, StepW: 100, MaxFan: 25})
	if _, ok := e["prepare"]; ok {
		t.Fatalf("prepare stage still estimated: %v", e)
	}
	if _, ok := e["floor"]; ok {
		t.Fatalf("floor stage still estimated (merged into the sweep): %v", e)
	}
	if _, ok := e["curves"]; ok {
		t.Fatalf("curves stage still estimated (merged into the sweep): %v", e)
	}
	if _, ok := e["wiring"]; ok {
		t.Fatalf("wiring stage still estimated (the sweep does the trim test): %v", e)
	}
	early := e["check"] + e["fixed"] + e["sweep"]
	if e["fixed"] == 0 || e["fixed"] > 4 || e["tuner"] == 0 || e["sweep"] < 5 || e["sweep"] > 8 || early > 20 {
		t.Fatalf("estimate: %v (early stages %d min)", e, early)
	}
}

// A wizard.json from before the prepare/wiring split (one "Check the wiring" stage) still loads as it was.
func TestWizLoadOldStages(t *testing.T) {
	dir := t.TempDir()
	old := ccfgPath
	ccfgPath = filepath.Join(dir, "core.json")
	wizMu.Lock()
	saved := wiz
	wizMu.Unlock()
	defer func() { ccfgPath = old; wizMu.Lock(); wiz = saved; wizMu.Unlock() }()
	js := `{"running":true,"stages":[{"key":"check","title":"Check the miner","status":"done"},
		{"key":"wiring","title":"Check the wiring","status":"done","detail":"50 % trim: 11.82 -> 11.21 V"},
		{"key":"floor","title":"Floor check","status":"running"}],"live":"x"}`
	if err := os.WriteFile(filepath.Join(dir, "wizard.json"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	wizLoad()
	s := wizSnap()
	if s.Running || len(s.Stages) != 3 || s.Stages[1].Key != "wiring" || s.Stages[1].Title != "Check the wiring" ||
		s.Stages[2].Status != "failed" || len(s.Events) != 0 {
		t.Fatalf("old state: %+v", s)
	}
}

// A wizard.json from before the floor + curves merge (separate "floor" and "curves" stages) still loads and keeps
// its floor, curve and stages for display.
func TestWizLoadFloorCurvesStages(t *testing.T) {
	dir := t.TempDir()
	old := ccfgPath
	ccfgPath = filepath.Join(dir, "core.json")
	wizMu.Lock()
	saved := wiz
	wizMu.Unlock()
	defer func() { ccfgPath = old; wizMu.Lock(); wiz = saved; wizMu.Unlock() }()
	js := `{"running":false,"floor_v":8.81,"stages":[{"key":"wiring","title":"Trim test","status":"done"},
		{"key":"floor","title":"Lowest voltage your resistor allows","status":"done","detail":"8.81 V at 100 % trim"},
		{"key":"curves","title":"Drive curve (test profile)","status":"done","detail":"960 W: 46 points, done"}],
		"curves":[{"watts":960,"stock_v":11.96,"points":[{"duty":30,"v":11.5},{"duty":75,"v":9.9}],"status":"done"}],
		"drive_points":[{"w":960,"v":8.81,"d":100,"b":"123","src":"floor","l":-1}]}`
	if err := os.WriteFile(filepath.Join(dir, "wizard.json"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	wizLoad()
	s := wizSnap()
	if len(s.Stages) != 3 || s.Stages[1].Key != "floor" || s.Stages[2].Key != "curves" || s.Floor != 8.81 ||
		len(s.Curves) != 1 || len(s.Curves[0].Points) != 2 || len(s.Points) != 1 {
		t.Fatalf("old state: %+v", s)
	}
}
