package main

// Extend (tune the added levels) and clearing a saved run.
//
// Extend = resume with a new range: every finished level whose job (watts, board set) is still in the new job
// list is kept, new jobs are added as waiting (low to high, renumbered), waiting jobs no longer in range are
// dropped, and finished levels outside the new range stay in the table marked Outside (never deleted). Then the run
// continues exactly like a resume. Max fan and chip target must be the saved run's: levels tuned with other limits
// are not comparable.
//
// clear_levels: chosen finished levels go back to waiting (their old result only in the event list); the next
// resume / extend tunes them again. clear_all: the saved run is wiped (preparation, rows, timelines, chip
// snapshots); the applied production table and Braiins' saved tunes and backups are not touched.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// jobMatches: a saved level belongs to job j (n2/n1 = any resolved set of that many boards).
func jobMatches(j wizJob, l wizLevel) bool {
	if l.Watts != j.W {
		return false
	}
	if strings.HasPrefix(j.B, "n") {
		return l.Boards == j.B || (len(l.Boards) == int(j.B[1]-'0') && l.Boards != "123")
	}
	return l.Boards == j.B
}

// extendLimits: why the new inputs cannot extend the saved run ("" = they can).
func extendLimits(s wizState, in wizInputs) string {
	switch {
	case s.Running:
		return "a run is in progress"
	case len(s.Stages) == 0:
		return "no saved run"
	case in.MaxFan != s.Inputs.MaxFan || in.ChipTarget != s.Inputs.ChipTarget:
		return fmt.Sprintf("max fan and chip temperature must stay as in the saved run (%d %%, %.0f °C): levels tuned with other limits are not comparable",
			s.Inputs.MaxFan, s.Inputs.ChipTarget)
	case in.StepW <= 0 || in.MaxW < in.MinW:
		return "bad range"
	}
	return ""
}

// extendPlan: the saved run's levels merged with the new job list. remap = old Index -> new Index (kept levels);
// added = levels that will run (new or unfinished), kept = finished levels kept in range, outside = finished
// levels kept outside the range.
type extendResult struct {
	Levels  []wizLevel
	Remap   map[int]int
	Added   int
	Kept    int
	Outside int
}

func extendPlan(s wizState, in wizInputs) (extendResult, error) {
	if why := extendLimits(s, in); why != "" {
		return extendResult{}, fmt.Errorf("%s", why)
	}
	used := make([]bool, len(s.Levels))
	type item struct {
		l     wizLevel
		old   int // old Index, -1 = new
		order int
	}
	var items []item
	for n, j := range in.jobs() {
		found := -1
		for i, l := range s.Levels {
			if !used[i] && jobMatches(j, l) && levelFinished(l) {
				found = i
				break
			}
		}
		if found < 0 { // an unfinished saved level for this job restarts like a new one
			for i, l := range s.Levels {
				if !used[i] && jobMatches(j, l) {
					used[i] = true
				}
			}
			items = append(items, item{wizLevel{Watts: j.W, Boards: j.B, Status: "waiting"}, -1, n})
			continue
		}
		used[found] = true
		l := s.Levels[found]
		l.Outside = false
		items = append(items, item{l, l.Index, n})
	}
	for i, l := range s.Levels { // finished levels outside the new range: kept, marked
		if !used[i] && levelFinished(l) {
			l.Outside = true
			items = append(items, item{l, l.Index, len(items) + 1000})
		}
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].l.Watts != items[b].l.Watts {
			return items[a].l.Watts < items[b].l.Watts
		}
		return items[a].order < items[b].order
	})
	res := extendResult{Remap: map[int]int{}}
	for i, it := range items {
		it.l.Index = i
		if it.old >= 0 {
			res.Remap[it.old] = i
		}
		switch {
		case !levelFinished(it.l):
			res.Added++
		case it.l.Outside:
			res.Outside++
		default:
			res.Kept++
		}
		res.Levels = append(res.Levels, it.l)
	}
	return res, nil
}

// applyExtend: the state for an extended run (the caller then starts it like a resume). Drive points follow the
// renumbering; points of dropped levels go.
func applyExtend(s *wizState, in wizInputs, x extendResult) {
	s.Inputs.MinW, s.Inputs.MaxW, s.Inputs.StepW, s.Inputs.FewerBoards = in.MinW, in.MaxW, in.StepW, in.FewerBoards
	s.Levels = x.Levels
	var pts []drivePt
	for _, p := range s.Points {
		if p.L >= 0 && p.Src == "step" {
			ni, ok := x.Remap[p.L]
			if !ok {
				continue
			}
			p.L = ni
		}
		pts = append(pts, p)
	}
	s.Points = pts
}

// remapChips: chip snapshots follow the renumbering (snapshots of dropped levels go).
func remapChips(m map[int][]chipSnap, remap map[int]int) map[int][]chipSnap {
	out := map[int][]chipSnap{}
	for old, v := range m {
		if ni, ok := remap[old]; ok {
			out[ni] = v
		}
	}
	return out
}

// levelKey: a level named by the page (clear_levels).
type levelKey struct {
	Watts  int    `json:"watts"`
	Boards string `json:"boards"`
}

// clearLevels: the named finished levels back to waiting; their results go to the event list. The search stage
// goes back to waiting so the run is resumable. Returns the cleared indices.
func clearLevels(s *wizState, keys []levelKey, now time.Time) []int {
	var out []int
	for i, l := range s.Levels {
		for _, k := range keys {
			if l.Watts == k.Watts && l.Boards == k.Boards && levelFinished(l) {
				s.Events = addWizEv(s.Events, wizEv{T: now.UnixMilli(), Kind: "level",
					Text: fmt.Sprintf("%d W, boards %s cleared (was: %s)", l.Watts, l.Boards, levelResult(l, nil))})
				s.Levels[i] = wizLevel{Watts: l.Watts, Boards: l.Boards, Status: "waiting", Index: l.Index}
				out = append(out, l.Index)
				break
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	var pts []drivePt
	for _, p := range s.Points {
		drop := false
		for _, i := range out {
			if p.Src == "step" && p.L == i {
				drop = true
			}
		}
		if !drop {
			pts = append(pts, p)
		}
	}
	s.Points = pts
	pickChosen(s.Levels)
	for i := range s.Stages {
		if s.Stages[i].Key == "search" && s.Stages[i].Status == "done" {
			s.Stages[i].Status, s.Stages[i].Detail = "waiting", fmt.Sprintf("%d levels cleared: resume tunes them again", len(out))
		}
	}
	s.FinalCheck = ""
	s.Message = fmt.Sprintf("%d levels cleared", len(out))
	return out
}

// clearRefused: clearing is refused while a run is in progress ("" = allowed).
func clearRefused(s wizState, setupBusy bool) string {
	if s.Running || setupBusy {
		return "refused while a run is in progress"
	}
	return ""
}

// clearAll: an empty saved run (a failed manual-mode restore still owed is kept).
func clearAll(s *wizState) { *s = wizState{ManualRestore: s.ManualRestore} }

// extendText: the button line, e.g. "Tune 3 added levels (≈3 h 00 min); keeps 8 finished".
func extendText(x extendResult, levelMin int) string {
	t := fmt.Sprintf("Tune %d added levels (≈%s); keeps %d finished", x.Added, hmText(x.Added*levelMin), x.Kept)
	if x.Outside > 0 {
		t += fmt.Sprintf(" + %d outside the range", x.Outside)
	}
	return t
}

func hmText(m int) string {
	if m >= 60 {
		return fmt.Sprintf("%d h %02d min", m/60, m%60)
	}
	return fmt.Sprintf("%d min", m)
}
