package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPhaseBeginClosesRunning(t *testing.T) {
	t0 := time.Unix(1000, 0)
	ps := phaseBegin(nil, wizPhase{Kind: "clean", Label: "Clean reset"}, t0)
	if len(ps) != 1 || ps[0].Status != "running" || ps[0].Start != 1000 || ps[0].End != 0 {
		t.Fatalf("begin: %+v", ps)
	}
	ps2 := phaseBegin(ps, wizPhase{Kind: "tune"}, t0.Add(5*time.Minute))
	if ps[0].Status != "running" {
		t.Fatal("phaseBegin modified the slice it was given (copy-on-write)")
	}
	if ps2[0].Status != "done" || ps2[0].End != 1300 || ps2[1].Status != "running" || phaseOpen(ps2) != 1 {
		t.Fatalf("second begin: %+v", ps2)
	}
	ps3 := phaseClose(ps2, "fail", "boom", t0.Add(6*time.Minute))
	if ps3[1].Status != "fail" || ps3[1].Detail != "boom" || ps3[1].End != 1360 || phaseOpen(ps3) != -1 {
		t.Fatalf("close: %+v", ps3)
	}
	if ps2[1].Status != "running" {
		t.Fatal("phaseClose modified its input")
	}
	// closing with nothing running changes nothing
	if ps4 := phaseClose(ps3, "done", "x", t0); ps4[1].Status != "fail" || ps4[1].Detail != "boom" {
		t.Fatalf("close without a running phase: %+v", ps4)
	}
}

func TestPhaseStepsAndLabels(t *testing.T) {
	ps := []wizPhase{{Kind: "clean"}, {Kind: "step"}, {Kind: "reset"}, {Kind: "step"}}
	if n := phaseSteps(ps); n != 2 {
		t.Fatalf("phaseSteps = %d", n)
	}
	if s := stepLabel(3, 57.84, false, 1); s != "Step 3: drive 57.8 %" {
		t.Fatalf("label %q", s)
	}
	if s := stepLabel(1, 40, true, 1); !strings.HasSuffix(s, "(short check)") {
		t.Fatalf("short label %q", s)
	}
	if s := stepLabel(5, 50, false, 1.03); !strings.HasPrefix(s, "Confirm at clocks ×1.030") {
		t.Fatalf("confirm label %q", s)
	}
}

func TestStepResult(t *testing.T) {
	st, d := stepResult(wizStep{V: 10.3, Drive: 59, THs: 50, JTH: 24.13, Pass: true, Full: true, Share: 0.98, MCR: 0.987})
	if st != "done" || !strings.HasPrefix(d, "pass") || !strings.Contains(d, "24.1 J/TH") || !strings.Contains(d, "mcr 98.7 %") {
		t.Fatalf("pass: %s %q", st, d)
	}
	st, d = stepResult(wizStep{V: 10.2, THs: 49, JTH: 24, NewWeak: 3, Full: true})
	if st != "fail" || !strings.Contains(d, "3 weak chips") {
		t.Fatalf("fail: %s %q", st, d)
	}
	if st, d = stepResult(wizStep{Invalid: "Setting voltage"}); st != "fail" || !strings.HasPrefix(d, "thrown out") {
		t.Fatalf("invalid: %s %q", st, d)
	}
	if st, d = stepResult(wizStep{Dropped: "board 2 at 40 %"}); st != "fail" || !strings.HasPrefix(d, "aborted") {
		t.Fatalf("dropped: %s %q", st, d)
	}
}

func TestLatchStatus(t *testing.T) {
	if latchStatus("latch check from the last step (...): clean -> answer cleared") != "done" {
		t.Fatal("cleared should be done")
	}
	if latchStatus("latch check: reset 5 min at the answer, back to stock: latched (2:14) -> the answer latches") != "fail" {
		t.Fatal("latched should be fail")
	}
}

func TestLevelEndPhases(t *testing.T) {
	now := time.Unix(5000, 0)
	ps := []wizPhase{{Kind: "step", Status: "running", Start: 4000}}
	out := levelEndPhases(ps, wizLevel{Watts: 1000}, errors.New("stopped"), now)
	if len(out) != 2 || out[0].Status != "fail" || out[0].Detail != "stopped" || out[1].Kind != "end" || out[1].Status != "fail" {
		t.Fatalf("error end: %+v", out)
	}
	out = levelEndPhases(ps, wizLevel{Watts: 1000, Status: "done", V: 10.4, Answer: "efficiency", EstW: 1003, EstJTH: 24.1}, nil, now)
	if out[0].Status != "done" || out[1].Status != "done" || !strings.Contains(out[1].Detail, "10.40 V") {
		t.Fatalf("done end: %+v", out)
	}
	if ps[0].Status != "running" {
		t.Fatal("levelEndPhases modified its input")
	}
}

// the levelRun hooks record into the working copy and push the same phases to the shared level
func TestLevelRunPhaseHooks(t *testing.T) {
	var shared wizLevel
	l := wizLevel{Watts: 1000}
	r := &levelRun{l: &l, scale: 1, upd: func(f func(*wizLevel)) { f(&shared) }}
	r.phase("clean", "Clean reset", cleanMin*time.Minute)
	r.phase("tune", "Braiins tunes 1170 W", braiinsMin*time.Minute)
	r.stepPhase(59.3, false)
	r.phaseMark("hold", 5*time.Minute)
	mark := l.Phases[2].Mark
	r.phaseMark("hold", 5*time.Minute) // same part: mark kept
	if l.Phases[2].Mark != mark || l.Phases[2].What != "hold" || l.Phases[2].Expect != 300 {
		t.Fatalf("mark: %+v", l.Phases[2])
	}
	r.stepDone(wizStep{V: 10.35, THs: 40, JTH: 25, NewWeak: 3, Full: true})
	r.resetPhase(false)
	r.phaseEndKind("latch") // not the running kind: nothing happens
	if phaseOpen(l.Phases) != 3 {
		t.Fatalf("phaseEndKind closed another kind: %+v", l.Phases)
	}
	r.phaseEndKind("reset")
	if len(shared.Phases) != 4 || shared.Phases[0].Status != "done" || shared.Phases[2].Status != "fail" ||
		shared.Phases[2].Label != "Step 1: drive 59.3 % → 10.35 V" || shared.Phases[3].Status != "done" {
		t.Fatalf("phases: %+v", shared.Phases)
	}
	r.latchPhase(true)
	r.latchDone("latch check from the last step (10.40 V avg, drive 58.0 %): clean -> answer cleared")
	res := l
	endLevel(r.upd, &res, nil)
	if n := len(shared.Phases); n != 6 || shared.Phases[4].Status != "done" || shared.Phases[5].Kind != "end" {
		t.Fatalf("end: %+v", shared.Phases)
	}
}

// old saved states (no phases) still load
func TestOldStateWithoutPhases(t *testing.T) {
	var s wizState
	if err := json.Unmarshal([]byte(`{"levels":[{"watts":1000,"status":"done","steps":[]}]}`), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Levels) != 1 || s.Levels[0].Phases != nil {
		t.Fatalf("%+v", s.Levels)
	}
	b, _ := json.Marshal(s.Levels[0])
	if strings.Contains(string(b), "phases") {
		t.Fatal("empty phases should be omitted")
	}
}
