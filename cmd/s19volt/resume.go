package main

// Tuned-levels table written level by level, the final cross-level check, partial apply, and resuming a stopped
// or interrupted setup run.
//
// Table: every level that finishes (done / unsafe) becomes a row at once (pickChosen: the best board count per
// wattage so far), persisted with the wizard state, so a stopped run keeps its finished rows. Apply can run at any
// time, even mid-run: it writes the rows that exist and leaves every other production row as it was (mergeTable).
// After the loop a short final pass (finalCheck) re-picks the board count and checks that the voltage rises
// with power; its notes go on the affected rows (wizLevel.Check).
//
// Resume: a run that was stopped or interrupted (not finished) restarts with its saved settings. The check stage
// always runs (it reads the miner identity); preparation (fixed profile, sweep, back to the tuner) is skipped when
// it completed and the miner identity matches, else it runs again ("redo_prep": always again, results replaced);
// a reusable preparation without droop points (droop.go) runs only the droop stage (+ tuner mode back).
// Finished levels are skipped; the interrupted one
// restarts from its clean reset. Braiins' saved tunes are moved aside only once per run (the first backup stays
// the restore point).

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// wizMiner: what preparation results depend on.
type wizMiner struct {
	Boards   int    `json:"boards"`   // hashboards bosminer reports
	Firmware string `json:"firmware"` // firmware version string
	Trim     bool   `json:"trim"`     // trim drive present
	MinW     int    `json:"min_w,omitempty"`
}

// same: the identity fields match (MinW is information only).
func (m wizMiner) same(o wizMiner) bool {
	return m.Boards == o.Boards && m.Firmware == o.Firmware && m.Trim == o.Trim
}

var bosVersionFile = "/etc/bos_version" // var: tests

// minerIdentity reads the miner's identity (board count from the API, firmware version, trim drive).
func minerIdentity(minW int) wizMiner {
	id := wizMiner{Trim: drv != nil, MinW: minW}
	if b, err := os.ReadFile(bosVersionFile); err == nil {
		id.Firmware = strings.TrimSpace(string(b))
	}
	if raw, err := bos("GET", "/miner/hw/hashboards", nil); err == nil {
		var hb struct {
			Hashboards []map[string]interface{} `json:"hashboards"`
		}
		if json.Unmarshal(raw, &hb) == nil {
			id.Boards = len(hb.Hashboards)
		}
	}
	return id
}

// prepStages: the one-time preparation stages after the check.
var prepStages = []string{"fixed", "sweep", "tuner"}

func stageStatus(s wizState, key string) string {
	for _, st := range s.Stages {
		if st.Key == key {
			return st.Status
		}
	}
	return ""
}

// prepReusable: preparation completed and the miner is the same -> skip it on resume (else why not).
func prepReusable(s wizState, id wizMiner) (bool, string) {
	for _, k := range prepStages {
		if stageStatus(s, k) != "done" {
			return false, "preparation did not finish"
		}
	}
	switch {
	case s.Floor <= 0 || len(s.Curves) == 0:
		return false, "no sweep results saved"
	case s.Miner == nil:
		return false, "no miner identity saved"
	case !s.Miner.same(id):
		return false, fmt.Sprintf("the miner changed (boards %d -> %d, firmware %q -> %q, trim %v -> %v)",
			s.Miner.Boards, id.Boards, s.Miner.Firmware, id.Firmware, s.Miner.Trim, id.Trim)
	}
	return true, ""
}

// resumePrep: whether a resumed run skips preparation (else why not). redo = "Resume and redo the sweep": it always
// runs again and replaces the saved preparation results (resetPrep); finished levels and the tunes backup stay.
func resumePrep(s wizState, id wizMiner, redo bool) (bool, string) {
	if redo {
		return false, "redo requested"
	}
	return prepReusable(s, id)
}

// tunesStep: what the tuner stage does with Braiins' saved tunes: keep this run's first backup (kept != ""; a
// resume, also with redo, never moves them again), move them now (move), or neither (reset tunes off).
func tunesStep(s wizState) (kept string, move bool) {
	if bk := tunesBackup(s); s.Inputs.ResetTunes && bk != "" {
		return bk, false
	}
	return "", needTunesMove(s)
}

// resetPrep: preparation runs again (resume on a changed miner, an unfinished preparation, or a redo). Step drive points
// of finished levels are kept; the tunes backup is remembered (never moved twice).
func resetPrep(s *wizState) {
	s.TunesBackup = tunesBackup(*s)
	for i := range s.Stages {
		for _, k := range append([]string{"droop"}, prepStages...) {
			if s.Stages[i].Key == k {
				s.Stages[i].Status, s.Stages[i].Detail, s.Stages[i].Started, s.Stages[i].Ended = "waiting", "", 0, 0
			}
		}
	}
	s.Fixed, s.Mod, s.Floor, s.Curves = nil, nil, 0, nil
	var pts []drivePt
	for _, p := range s.Points {
		if p.Src == "step" {
			pts = append(pts, p)
		}
	}
	s.Points = pts
}

// tunesBackup: where this run moved Braiins' saved tunes ("" = not moved yet). Older states only have it in the
// tuner stage's detail.
func tunesBackup(s wizState) string {
	if s.TunesBackup != "" {
		return s.TunesBackup
	}
	for _, st := range s.Stages {
		if st.Key == "tuner" && st.Status == "done" {
			if i := strings.Index(st.Detail, "saved Braiins tunes -> "); i >= 0 {
				bk := st.Detail[i+len("saved Braiins tunes -> "):]
				if j := strings.Index(bk, " ("); j >= 0 {
					bk = bk[:j]
				}
				return bk
			}
		}
	}
	return ""
}

// needTunesMove: the tuner stage moves the saved tunes aside only if this run has not done it yet.
func needTunesMove(s wizState) bool { return s.Inputs.ResetTunes && tunesBackup(s) == "" }

// levelFinished: a level that needs no more work on resume.
func levelFinished(l wizLevel) bool {
	switch l.Status {
	case "done", "unsafe", "too_hot", "skipped":
		return true
	case "failed": // a non-fatal failure finished the level; a stop / power cycle did not (older states)
		return !interruptNote(l.Note)
	}
	return false
}

func interruptNote(n string) bool {
	return n == "stopped" || n == errPowerCycle.Error() || strings.HasPrefix(n, "interrupted")
}

// runFinished: the search stage completed (nothing to resume).
func runFinished(s wizState) bool { return stageStatus(s, "search") == "done" }

// resumable: a saved run that can be resumed, else why not.
func resumable(s wizState) (bool, string) {
	switch {
	case s.Running:
		return false, "a run is in progress"
	case len(s.Stages) == 0:
		return false, "no saved run"
	case runFinished(s):
		return false, "the saved run finished"
	case s.Inputs.StepW <= 0:
		return false, "the saved run has no settings"
	}
	return true, ""
}

// ensureStages: a saved run from before a stage existed (e.g. droop) gets it, waiting, in its place.
func ensureStages(s *wizState) {
	have := map[string]bool{}
	for _, st := range s.Stages {
		have[st.Key] = true
	}
	est := estMinutes(s.Inputs)
	var out []wizStage
	for _, t := range wizStageTitles {
		if !have[t[0]] {
			out = append(out, wizStage{Key: t[0], Title: t[1], Status: "waiting", EstMin: est[t[0]]})
			continue
		}
		for _, st := range s.Stages {
			if st.Key == t[0] {
				out = append(out, st)
			}
		}
	}
	for _, st := range s.Stages { // keys of older runs (wiring, floor, curves...) stay, at the end
		known := false
		for _, t := range wizStageTitles {
			known = known || t[0] == st.Key
		}
		if !known {
			out = append(out, st)
		}
	}
	s.Stages = out
}

// droopOnlyStages: a resume that runs only the droop stage (preparation reusable, no droop points): droop and the
// tuner stage (tuner mode back after the droop's manual mode) wait again.
func droopOnlyStages(s *wizState) {
	for i := range s.Stages {
		if k := s.Stages[i].Key; k == "droop" || k == "tuner" {
			s.Stages[i].Status, s.Stages[i].Detail, s.Stages[i].Started, s.Stages[i].Ended = "waiting", "", 0, 0
		}
	}
}

// resumePlan: what a resumed run does before the search: "full" (preparation again), "droop" (only the droop stage
// + the tuner stage: preparation reusable, no droop points yet) or "none"; why = the reason preparation runs.
func resumePlan(s wizState, id wizMiner, redo bool) (plan, why string) {
	if ok, why := resumePrep(s, id, redo); !ok {
		return "full", why
	}
	if droopNeeded(s) {
		return "droop", ""
	}
	return "none", ""
}

// prepareResume: the state for a resumed run: running again, the check stage and the search stage back to
// waiting, every unfinished level reset to waiting (it restarts from its clean reset). Returns the reset levels.
func prepareResume(s *wizState, now int64) []int {
	s.Running, s.Ended, s.Message, s.FinalCheck = true, 0, "", ""
	s.TunesBackup = tunesBackup(*s)
	ensureStages(s)
	for i := range s.Stages {
		k := s.Stages[i].Key
		if k == "check" || k == "search" || (s.Stages[i].Status != "done" && k != "table") {
			s.Stages[i].Status, s.Stages[i].Detail, s.Stages[i].Started, s.Stages[i].Ended = "waiting", "", 0, 0
		}
	}
	var reset []int
	for i, l := range s.Levels {
		if levelFinished(l) {
			continue
		}
		s.Levels[i] = wizLevel{Watts: l.Watts, Boards: l.Boards, Status: "waiting", Index: l.Index}
		reset = append(reset, i)
		var pts []drivePt
		for _, p := range s.Points {
			if !(p.Src == "step" && p.L == l.Index) {
				pts = append(pts, p)
			}
		}
		s.Points = pts
	}
	s.Resumed = append(s.Resumed, now)
	return reset
}

// resumeSummary: one line for the Resume button.
func resumeSummary(s wizState) string {
	prep := "preparation runs again"
	if ok, _ := prepReusable(s, *orZero(s.Miner)); ok {
		prep = "preparation done"
		if droopNeeded(s) {
			prep += " (the droop stage runs)"
		}
	}
	done, next := 0, ""
	for _, l := range s.Levels {
		if levelFinished(l) {
			done++
		} else if next == "" {
			next = fmt.Sprintf("%d W", l.Watts)
		}
	}
	n := len(s.Levels)
	if n == 0 {
		n = len(s.Inputs.jobs())
		return fmt.Sprintf("Resume: %s, 0 of %d levels done", prep, n)
	}
	out := fmt.Sprintf("Resume: %s, %d of %d levels done", prep, done, n)
	if next != "" {
		out += ", restarts " + next
	}
	return out
}

func orZero(m *wizMiner) *wizMiner {
	if m == nil {
		return &wizMiner{}
	}
	return m
}

// pickChosen: the best board count per wattage among finished levels (done / unsafe), by estimated J/TH (stock
// J/TH when no trimmed answer). Run after every level (the row appears at once) and in the final check.
func pickChosen(levels []wizLevel) {
	best := map[int]int{}
	jth := func(l wizLevel) float64 {
		if l.EstJTH > 0 {
			return l.EstJTH
		}
		return l.StockJT
	}
	for i := range levels {
		levels[i].Chosen = false
		l := levels[i]
		if l.Status != "done" && l.Status != "unsafe" {
			continue
		}
		if j, ok := best[l.Watts]; !ok || jth(l) < jth(levels[j]) {
			best[l.Watts] = i
		}
	}
	for _, i := range best {
		levels[i].Chosen = true
	}
}

// finalCheck: the short pass after the loop: board-count pick, then the voltage must rise with power among the
// kept rows (same board set); a drop is noted on the higher row. Returns the summary line.
func finalCheck(levels []wizLevel) string {
	pickChosen(levels)
	for i := range levels {
		levels[i].Check = ""
	}
	var rows []int
	for i, l := range levels {
		if l.Chosen && l.V > 0 {
			rows = append(rows, i)
		}
	}
	sort.SliceStable(rows, func(a, b int) bool { return levels[rows[a]].Watts < levels[rows[b]].Watts })
	notes := 0
	last := map[string]int{} // board set -> previous kept row
	for _, i := range rows {
		l := &levels[i]
		if j, ok := last[l.Boards]; ok && l.V < levels[j].V-1e-6 {
			l.Check = fmt.Sprintf("voltage lower than at %d W (%.2f V): check this level", levels[j].Watts, levels[j].V)
			notes++
		}
		last[l.Boards] = i
	}
	kept := 0
	for _, l := range levels {
		if l.Chosen {
			kept++
		}
	}
	if notes == 0 {
		return fmt.Sprintf("final check: %d rows, voltage rises with power ✓", kept)
	}
	return fmt.Sprintf("final check: %d rows, %d noted (see the rows)", kept, notes)
}

// wizTable: the tuned-levels rows that exist now (chosen finished levels).
func wizTable(s wizState) map[string]levelEntry {
	t := map[string]levelEntry{}
	for _, l := range s.Levels {
		if l.Chosen && (l.Status == "done" || l.Status == "unsafe") {
			t[strconv.Itoa(l.Watts)] = levelEntry{V: l.V, D: l.Drive, B: l.Boards, T: l.Target, E: math.Round(l.EstJTH*10) / 10, F: l.Clock,
				W: l.EstW, S: math.Round(l.StockJT*10) / 10, SW: l.StockW, Fan: l.Fan}
		}
	}
	return t
}

// mergeTable: production rows with the run's rows written over them; rows the run has not produced are kept.
func mergeTable(prod, run map[string]levelEntry) map[string]levelEntry {
	out := map[string]levelEntry{}
	for k, v := range prod {
		out[k] = v
	}
	for k, v := range run {
		out[k] = v
	}
	return out
}
