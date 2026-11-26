package main

import (
	"strings"
	"testing"
)

func doneStages(keys ...string) []wizStage {
	var st []wizStage
	for _, k := range []string{"check", "fixed", "sweep", "tuner", "search"} {
		s := wizStage{Key: k, Status: "waiting"}
		for _, d := range keys {
			if d == k {
				s.Status = "done"
			}
		}
		st = append(st, s)
	}
	return st
}

var testMiner = wizMiner{Boards: 3, Firmware: "2026.11-plus", Trim: true}

func stoppedRun() wizState {
	m := testMiner
	return wizState{
		Inputs: wizInputs{MinW: 1000, MaxW: 1300, StepW: 100, MaxFan: 30, ChipTarget: 74, ResetTunes: true},
		Stages: append(doneStages("check", "fixed", "sweep", "tuner")[:4], wizStage{Key: "search", Status: "failed", Detail: "stopped by user"}),
		Floor:  8.8, Curves: []wizCurve{{Watts: 950}}, Miner: &m,
		TunesBackup: "/etc/s19volt/backup/bosminer-autotune-x.json",
		Points:      []drivePt{{Src: "sweep"}, {Src: "floor"}, {Src: "step", L: 0}, {Src: "step", L: 1}},
		Levels: []wizLevel{
			{Watts: 1000, Boards: "123", Status: "done", V: 10.4, EstJTH: 24, Index: 0, Phases: []wizPhase{{Kind: "end"}}},
			{Watts: 1100, Boards: "123", Status: "interrupted", Note: "stopped", Index: 1, Steps: []wizStep{{V: 10.5}}, Phases: []wizPhase{{Kind: "clean"}}},
			{Watts: 1200, Boards: "123", Status: "waiting", Index: 2},
			{Watts: 1300, Boards: "123", Status: "waiting", Index: 3},
		},
	}
}

func TestResumable(t *testing.T) {
	s := stoppedRun()
	if ok, why := resumable(s); !ok {
		t.Fatalf("stopped run not resumable: %s", why)
	}
	f := stoppedRun()
	f.Stages[4].Status = "done"
	if ok, _ := resumable(f); ok {
		t.Fatal("a finished run must not be resumable")
	}
	r := stoppedRun()
	r.Running = true
	if ok, _ := resumable(r); ok {
		t.Fatal("a running run must not be resumable")
	}
	if ok, _ := resumable(wizState{}); ok {
		t.Fatal("no saved run must not be resumable")
	}
}

func TestPrepReusable(t *testing.T) {
	s := stoppedRun()
	if ok, why := prepReusable(s, testMiner); !ok {
		t.Fatalf("same miner: %s", why)
	}
	other := testMiner
	other.Firmware = "2026.12-plus"
	if ok, why := prepReusable(s, other); ok || !strings.Contains(why, "miner changed") {
		t.Fatalf("firmware change must rerun preparation: %v %s", ok, why)
	}
	other = testMiner
	other.Boards = 2
	if ok, _ := prepReusable(s, other); ok {
		t.Fatal("board count change must rerun preparation")
	}
	other = testMiner
	other.MinW = 900 // information only
	if ok, _ := prepReusable(s, other); !ok {
		t.Fatal("MinW is not part of the identity")
	}
	u := stoppedRun()
	u.Stages[2].Status = "failed"
	if ok, _ := prepReusable(u, testMiner); ok {
		t.Fatal("an unfinished preparation must rerun")
	}
}

func TestPrepareResumeSkipsFinishedLevels(t *testing.T) {
	s := stoppedRun()
	reset := prepareResume(&s, 12345)
	if len(reset) != 3 || reset[0] != 1 {
		t.Fatalf("reset levels %v", reset)
	}
	if s.Levels[0].Status != "done" || len(s.Levels[0].Phases) != 1 {
		t.Fatal("finished level must be kept")
	}
	if l := s.Levels[1]; l.Status != "waiting" || l.Steps != nil || l.Phases != nil || l.Index != 1 || l.Watts != 1100 {
		t.Fatalf("interrupted level must restart from scratch: %+v", l)
	}
	if !s.Running || len(s.Resumed) != 1 {
		t.Fatal("resume must mark the run running")
	}
	for _, st := range s.Stages {
		if (st.Key == "check" || st.Key == "search") && st.Status != "waiting" {
			t.Fatalf("%s should be waiting again", st.Key)
		}
		if (st.Key == "fixed" || st.Key == "sweep" || st.Key == "tuner") && st.Status != "done" {
			t.Fatalf("%s should stay done until the identity check", st.Key)
		}
	}
	for _, p := range s.Points {
		if p.Src == "step" && p.L == 1 {
			t.Fatal("the interrupted level's step points must go")
		}
	}
	// an older state: stopped level stored as failed/"stopped"
	o := stoppedRun()
	o.Levels[1].Status = "failed"
	if levelFinished(o.Levels[1]) {
		t.Fatal("failed with note stopped = interrupted")
	}
	o.Levels[1].Note = "no board ranking"
	if !levelFinished(o.Levels[1]) {
		t.Fatal("a real failure is finished")
	}
}

func TestResetPrepKeepsTunesBackupAndSteps(t *testing.T) {
	s := stoppedRun()
	resetPrep(&s)
	if s.Floor != 0 || s.Curves != nil || s.Mod != nil {
		t.Fatal("preparation results must be cleared")
	}
	if len(s.Points) != 2 || s.Points[0].Src != "step" {
		t.Fatalf("only step points kept: %+v", s.Points)
	}
	if needTunesMove(s) {
		t.Fatal("the tunes backup must never be rewritten on a resume")
	}
	if s.TunesBackup != "/etc/s19volt/backup/bosminer-autotune-x.json" {
		t.Fatal("backup path lost")
	}
	// older state: backup only in the tuner stage's detail
	o := stoppedRun()
	o.TunesBackup = ""
	o.Stages[3].Detail = "tuner mode at 1500 W; saved Braiins tunes -> /etc/s19volt/backup/b.json (restore: copy it back)"
	if bk := tunesBackup(o); bk != "/etc/s19volt/backup/b.json" {
		t.Fatalf("backup from detail: %q", bk)
	}
	resetPrep(&o)
	if needTunesMove(o) || o.TunesBackup != "/etc/s19volt/backup/b.json" {
		t.Fatal("older state: the backup must be kept after a preparation reset")
	}
	// a run stopped before the tuner stage moved anything: the move is still owed
	n := stoppedRun()
	n.TunesBackup = ""
	n.Stages[3].Status = "waiting"
	if !needTunesMove(n) {
		t.Fatal("tunes not moved yet: the move is still needed")
	}
}

func TestResumeSummary(t *testing.T) {
	s := stoppedRun()
	got := resumeSummary(s)
	if got != "Resume: preparation done, 1 of 4 levels done, restarts 1100 W" {
		t.Fatalf("%q", got)
	}
	s.Miner = nil
	if !strings.Contains(resumeSummary(s), "preparation runs again") {
		t.Fatal("no identity: preparation runs again")
	}
}

// each finished level writes its row at once; unfinished levels have none
func TestPerLevelRows(t *testing.T) {
	s := stoppedRun()
	pickChosen(s.Levels)
	tb := wizTable(s)
	if len(tb) != 1 || tb["1000"].V != 10.4 {
		t.Fatalf("rows after level 1: %+v", tb)
	}
	s.Levels[1].Status, s.Levels[1].V, s.Levels[1].EstJTH = "done", 10.5, 24.5
	pickChosen(s.Levels)
	if tb = wizTable(s); len(tb) != 2 || tb["1100"].V != 10.5 {
		t.Fatalf("rows after level 2: %+v", tb)
	}
	// two board counts at one wattage: the better J/TH is the row
	s.Levels = append(s.Levels, wizLevel{Watts: 1100, Boards: "13", Status: "done", V: 10.2, EstJTH: 23.9})
	pickChosen(s.Levels)
	if tb = wizTable(s); tb["1100"].B != "13" {
		t.Fatalf("board pick: %+v", tb["1100"])
	}
}

func TestPartialApplyMerge(t *testing.T) {
	prod := map[string]levelEntry{"1000": {V: 11}, "1100": {V: 11.1}, "1500": {V: 11.5}}
	run := map[string]levelEntry{"1000": {V: 10.4}}
	got := mergeTable(prod, run)
	if len(got) != 3 || got["1000"].V != 10.4 || got["1100"].V != 11.1 || got["1500"].V != 11.5 {
		t.Fatalf("merge: %+v", got)
	}
	if prod["1000"].V != 11 {
		t.Fatal("mergeTable modified production")
	}
}

func TestFinalCheck(t *testing.T) {
	ls := []wizLevel{
		{Watts: 1000, Boards: "123", Status: "done", V: 10.5, EstJTH: 24},
		{Watts: 1100, Boards: "123", Status: "done", V: 10.4, EstJTH: 24},
		{Watts: 1200, Boards: "123", Status: "done", V: 10.6, EstJTH: 24},
	}
	line := finalCheck(ls)
	if ls[1].Check == "" || ls[0].Check != "" || ls[2].Check != "" || !strings.Contains(line, "1 noted") {
		t.Fatalf("%q %+v", line, ls)
	}
	ls[1].V = 10.55
	if line = finalCheck(ls); ls[1].Check != "" || !strings.Contains(line, "✓") {
		t.Fatalf("%q", line)
	}
}

// "Resume and redo the sweep": preparation runs again even when it could be reused; its saved results are replaced;
// finished levels and their rows stay; Braiins' saved tunes are not moved again (the first backup stays).
func TestResumeRedoPrep(t *testing.T) {
	s := stoppedRun()
	if ok, _ := prepReusable(s, testMiner); !ok {
		t.Fatal("fixture: preparation reusable")
	}
	if ok, why := resumePrep(s, testMiner, false); !ok {
		t.Fatalf("plain resume must skip: %s", why)
	}
	ok, why := resumePrep(s, testMiner, true)
	if ok || why != "redo requested" {
		t.Fatalf("redo must force preparation: %v %s", ok, why)
	}
	// the handler's order: prepareResume, then (wizRun) resetPrep because preparation runs
	reset := prepareResume(&s, 1)
	resetPrep(&s)
	if s.Floor != 0 || s.Curves != nil || s.Mod != nil || s.Fixed != nil {
		t.Fatal("saved preparation results must be replaced")
	}
	for _, k := range prepStages {
		if stageStatus(s, k) != "waiting" {
			t.Fatalf("stage %s must run again", k)
		}
	}
	if s.Levels[0].Status != "done" || s.Levels[0].V != 10.4 || len(reset) != 3 || reset[0] != 1 {
		t.Fatalf("finished level kept, interrupted restarts: %+v %v", s.Levels[0], reset)
	}
	if len(wizTable(stoppedRunChosen(s))) != 1 {
		t.Fatal("the finished level's row must stay")
	}
	kept, move := tunesStep(s)
	if move || kept != "/etc/s19volt/backup/bosminer-autotune-x.json" {
		t.Fatalf("tunes must not move again: kept %q move %v", kept, move)
	}
	// a run that never moved them still owes the move (redo does not skip it)
	n := stoppedRun()
	n.TunesBackup = ""
	n.Stages[3].Status = "waiting"
	resetPrep(&n)
	if kept, move := tunesStep(n); kept != "" || !move {
		t.Fatalf("move still owed: %q %v", kept, move)
	}
}

func stoppedRunChosen(s wizState) wizState {
	pickChosen(s.Levels)
	return s
}
