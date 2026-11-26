package main

import (
	"strings"
	"testing"
	"time"
)

// a finished run 1000-1300 W step 100 (3 boards)
func finishedRun() wizState {
	s := stoppedRun()
	s.Stages[4].Status = "done"
	for i := range s.Levels {
		s.Levels[i].Status, s.Levels[i].V, s.Levels[i].EstJTH, s.Levels[i].Note = "done", 10.4+float64(i)/10, 24, ""
		s.Levels[i].Steps = []wizStep{{V: 10.4}}
	}
	pickChosen(s.Levels)
	return s
}

func watts(ls []wizLevel) []int {
	var out []int
	for _, l := range ls {
		out = append(out, l.Watts)
	}
	return out
}

func TestExtendPlan(t *testing.T) {
	s := finishedRun()
	in := s.Inputs
	in.MinW, in.MaxW = 800, 1500 // new below and above
	x, err := extendPlan(s, in)
	if err != nil {
		t.Fatal(err)
	}
	if got := watts(x.Levels); len(got) != 8 || got[0] != 800 || got[7] != 1500 {
		t.Fatalf("levels %v", got)
	}
	if x.Added != 4 || x.Kept != 4 || x.Outside != 0 {
		t.Fatalf("added %d kept %d outside %d", x.Added, x.Kept, x.Outside)
	}
	for i, l := range x.Levels {
		if l.Index != i {
			t.Fatal("index not renumbered")
		}
		if l.Watts == 1000 && (l.Status != "done" || len(l.Steps) != 1) {
			t.Fatal("finished level not kept")
		}
	}
	if x.Remap[0] != 2 || x.Remap[3] != 5 {
		t.Fatalf("remap %v", x.Remap)
	}
	// in between: step 50 adds 1050, 1150, 1250
	in = s.Inputs
	in.StepW = 50
	if x, _ = extendPlan(s, in); x.Added != 3 || x.Kept != 4 {
		t.Fatalf("in between: added %d kept %d %v", x.Added, x.Kept, watts(x.Levels))
	}
	// narrower: finished rows outside kept and marked
	in = s.Inputs
	in.MinW, in.MaxW = 1100, 1200
	x, _ = extendPlan(s, in)
	if x.Added != 0 || x.Kept != 2 || x.Outside != 2 || len(x.Levels) != 4 || !x.Levels[0].Outside || x.Levels[1].Outside {
		t.Fatalf("narrower: %+v", x)
	}
	// limits changed: refused
	in = s.Inputs
	in.MaxFan++
	if _, err = extendPlan(s, in); err == nil || !strings.Contains(err.Error(), "max fan") {
		t.Fatalf("max fan change: %v", err)
	}
	in = s.Inputs
	in.ChipTarget = 70
	if _, err = extendPlan(s, in); err == nil {
		t.Fatal("chip target change must be refused")
	}
	r := s
	r.Running = true
	if _, err = extendPlan(r, s.Inputs); err == nil {
		t.Fatal("refused while running")
	}
}

func TestExtendDropsWaitingOutOfRange(t *testing.T) {
	s := stoppedRun() // 1000 done, 1100 interrupted, 1200/1300 waiting
	in := s.Inputs
	in.MaxW = 1100
	x, err := extendPlan(s, in)
	if err != nil {
		t.Fatal(err)
	}
	if got := watts(x.Levels); len(got) != 2 || got[1] != 1100 || x.Levels[1].Status != "waiting" || x.Levels[1].Steps != nil {
		t.Fatalf("%v %+v", got, x.Levels)
	}
	applyExtend(&s, in, x)
	for _, p := range s.Points {
		if p.Src == "step" && p.L == 1 {
			t.Fatal("points of the restarted level must go")
		}
	}
	if s.Inputs.MaxW != 1100 {
		t.Fatal("inputs not updated")
	}
	m := remapChips(map[int][]chipSnap{0: {{Label: "a"}}, 3: {{Label: "b"}}}, x.Remap)
	if len(m) != 1 || m[0][0].Label != "a" {
		t.Fatalf("chips %v", m)
	}
}

func TestExtendFewerBoardsMatch(t *testing.T) {
	if !jobMatches(wizJob{500, "n2"}, wizLevel{Watts: 500, Boards: "13"}) || jobMatches(wizJob{500, "n2"}, wizLevel{Watts: 500, Boards: "123"}) {
		t.Fatal("n2 must match a resolved 2-board set only")
	}
}

func TestClearLevels(t *testing.T) {
	s := finishedRun()
	if ok, _ := resumable(s); ok {
		t.Fatal("finished run")
	}
	got := clearLevels(&s, []levelKey{{1100, "123"}, {1300, "123"}}, time.Unix(100, 0))
	if len(got) != 2 {
		t.Fatalf("cleared %v", got)
	}
	if s.Levels[1].Status != "waiting" || s.Levels[1].Steps != nil || s.Levels[1].Chosen {
		t.Fatal("cleared level must be waiting with no result")
	}
	if _, ok := wizTable(s)["1100"]; ok {
		t.Fatal("cleared row still in the table")
	}
	if len(s.Events) != 2 || !strings.Contains(s.Events[0].Text, "cleared (was: done") {
		t.Fatalf("events %+v", s.Events)
	}
	ok, _ := resumable(s)
	if !ok {
		t.Fatal("clearing must make the run resumable")
	}
	reset := prepareResume(&s, 1)
	if len(reset) != 2 || reset[0] != 1 || reset[1] != 3 {
		t.Fatalf("resume runs %v", reset)
	}
	// extend re-tunes cleared levels too
	s2 := finishedRun()
	clearLevels(&s2, []levelKey{{1200, "123"}}, time.Now())
	if x, _ := extendPlan(s2, s2.Inputs); x.Added != 1 || x.Kept != 3 {
		t.Fatalf("extend after clear: %+v", x)
	}
}

func TestClearAll(t *testing.T) {
	s := finishedRun()
	s.ManualRestore = 1500
	clearAll(&s)
	if ok, _ := resumable(s); ok {
		t.Fatal("cleared run must not be resumable")
	}
	if len(s.Levels) != 0 || s.Floor != 0 || s.TunesBackup != "" || s.ManualRestore != 1500 {
		t.Fatalf("%+v", s)
	}
}

func TestExtendText(t *testing.T) {
	if got := extendText(extendResult{Added: 3, Kept: 8}, 60); got != "Tune 3 added levels (≈3 h 00 min); keeps 8 finished" {
		t.Fatalf("%q", got)
	}
}

func TestClearRefusedWhileRunning(t *testing.T) {
	s := finishedRun()
	if clearRefused(s, false) != "" {
		t.Fatal("allowed when idle")
	}
	s.Running = true
	if clearRefused(s, false) == "" || clearRefused(finishedRun(), true) == "" {
		t.Fatal("clear_levels / clear_all must be refused while running")
	}
}
