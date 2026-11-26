package main

// Per-level phase timeline for the setup monitor: every level records its phases (clean reset, Braiins' fresh
// tune, settled round + stock baseline, each step, soft resets, settled-round waits, latch checks, the level's
// result) with start/end times, status and a one-line result. The page draws them as a timeline per level.
//
// The helpers are copy-on-write: they never modify a slice that was already handed to the shared wizard state
// (wizSnap marshals it under wizMu while the search goroutine keeps recording).

import (
	"fmt"
	"strings"
	"time"
)

// wizPhase: one phase of a level.
type wizPhase struct {
	Kind   string `json:"kind"`   // clean | tune | baseline | step | reset | settle | latch | end
	Label  string `json:"label"`  // e.g. "Step 2: drive 57.8 % → 10.46 V"
	Start  int64  `json:"start"`  // unix s
	End    int64  `json:"end"`    // unix s (0 = running)
	Status string `json:"status"` // running | done | fail
	Detail string `json:"detail,omitempty"`
	Expect int    `json:"expect,omitempty"`    // expected seconds of the running part (from Mark, else from Start)
	Mark   int64  `json:"mark,omitempty"`      // unix s: the measured part began (e.g. the hold after the glide)
	What   string `json:"mark_what,omitempty"` // what Mark/Expect count, e.g. "hold"
}

// phaseOpen: index of the running phase (the last one, if running), -1 if none.
func phaseOpen(ps []wizPhase) int {
	if n := len(ps); n > 0 && ps[n-1].Status == "running" {
		return n - 1
	}
	return -1
}

// phaseEdit: a copy of ps with f applied to the running phase (unchanged copy if none).
func phaseEdit(ps []wizPhase, f func(p *wizPhase)) []wizPhase {
	out := append([]wizPhase(nil), ps...)
	if i := phaseOpen(out); i >= 0 {
		f(&out[i])
	}
	return out
}

// phaseClose: the running phase ends at now with status (detail kept if empty).
func phaseClose(ps []wizPhase, status, detail string, now time.Time) []wizPhase {
	return phaseEdit(ps, func(p *wizPhase) {
		p.End, p.Status = now.Unix(), status
		if detail != "" {
			p.Detail = detail
		}
	})
}

// phaseBegin: closes a running phase as done and starts p at now.
func phaseBegin(ps []wizPhase, p wizPhase, now time.Time) []wizPhase {
	out := phaseClose(ps, "done", "", now)
	p.Start, p.End, p.Status = now.Unix(), 0, "running"
	return append(out, p)
}

// phaseSteps: how many step phases the level has (the next step's number - 1).
func phaseSteps(ps []wizPhase) int {
	n := 0
	for _, p := range ps {
		if p.Kind == "step" {
			n++
		}
	}
	return n
}

// stepLabel: a step phase's label before its result.
func stepLabel(n int, d float64, short bool, scale float64) string {
	s := fmt.Sprintf("Step %d: drive %.1f %%", n, d)
	if scale != 1 && scale != 0 {
		s = fmt.Sprintf("Confirm at clocks ×%.3f: drive %.1f %%", scale, d)
	}
	if short {
		s += " (short check)"
	}
	return s
}

// stepResult: a finished step's status and one-line result for the timeline.
func stepResult(st wizStep) (status, detail string) {
	switch {
	case st.Invalid != "":
		return "fail", "thrown out (redone): " + st.Invalid
	case st.Dropped != "":
		return "fail", "aborted: " + st.Dropped
	}
	var parts []string
	pass := st.usable() && st.Pass
	if pass {
		parts = append(parts, "pass")
	} else {
		parts = append(parts, "fail")
	}
	if st.NewWeak > 0 || !pass {
		w := fmt.Sprintf("%d weak chips", st.NewWeak)
		if st.NewWeak == 1 {
			w = "1 weak chip"
		}
		parts = append(parts, w)
	}
	if st.Share > 0 && st.Share < 1 {
		parts = append(parts, fmt.Sprintf("slowest board %.0f %%", st.Share*100))
	}
	if st.MCR > 0 {
		parts = append(parts, fmt.Sprintf("mcr %.1f %%", st.MCR*100))
	}
	if st.JTH > 0 {
		parts = append(parts, fmt.Sprintf("%.1f J/TH", st.JTH))
	}
	if st.Full {
		parts = append(parts, fmt.Sprintf("%d-min hold", holdMin))
	} else {
		parts = append(parts, "short check")
	}
	status = "fail"
	if pass {
		status = "done"
	}
	return status, strings.Join(parts, " · ")
}

// latchStatus: a latch check's record line -> done (answer cleared) | fail.
func latchStatus(s string) string {
	if strings.HasSuffix(s, "answer cleared") {
		return "done"
	}
	return "fail"
}

// levelEndPhases: closes a running phase (fail with err, or done) and appends the level's result.
func levelEndPhases(ps []wizPhase, res wizLevel, err error, now time.Time) []wizPhase {
	st := "done"
	if err != nil {
		st = "fail"
	}
	out := phaseClose(ps, st, errText(err), now)
	if err == nil && res.Status != "done" {
		st = "fail"
	}
	return append(out, wizPhase{Kind: "end", Label: "Level result", Start: now.Unix(), End: now.Unix(), Status: st,
		Detail: levelResult(res, err)})
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---- levelRun hooks (the search goroutine; r.l is the level's working copy, pushed to the shared state) ----

func (r *levelRun) setPhases(ps []wizPhase) {
	r.l.Phases = ps
	if r.upd != nil {
		r.upd(func(x *wizLevel) { x.Phases = ps })
	}
}

// phase starts a phase (a running one ends as done); expect = its usual duration (0 = unknown).
func (r *levelRun) phase(kind, label string, expect time.Duration) {
	r.setPhases(phaseBegin(r.l.Phases, wizPhase{Kind: kind, Label: label, Expect: int(expect.Seconds())}, time.Now()))
}

// phaseDone ends the running phase.
func (r *levelRun) phaseDone(status, detail string) {
	r.setPhases(phaseClose(r.l.Phases, status, detail, time.Now()))
}

// phaseEndKind ends the running phase if it is of kind (done, or fail when a stop was requested).
func (r *levelRun) phaseEndKind(kind string) {
	if i := phaseOpen(r.l.Phases); i < 0 || r.l.Phases[i].Kind != kind {
		return
	}
	if stopRequested() {
		r.phaseDone("fail", "stopped")
		return
	}
	r.phaseDone("done", "")
}

// phaseMark: the measured part (what, e.g. "hold") of the running phase starts now, expected to last expect; a
// mark already set for the same part is kept (only the expected time changes, e.g. a short check extended).
func (r *levelRun) phaseMark(what string, expect time.Duration) {
	now := time.Now().Unix()
	r.setPhases(phaseEdit(r.l.Phases, func(p *wizPhase) {
		if p.Mark == 0 || p.What != what {
			p.Mark, p.What = now, what
		}
		p.Expect = int(expect.Seconds())
	}))
}

// stepPhase: a step (glide + hold) at drive d starts.
func (r *levelRun) stepPhase(d float64, short bool) {
	m := holdMin
	if short {
		m = shortMin
	}
	r.phase("step", stepLabel(phaseSteps(r.l.Phases)+1, d, short, r.scale), time.Duration(m)*time.Minute)
}

// stepDone: the running step ends with its verdict (the label gets its mean voltage).
func (r *levelRun) stepDone(st wizStep) {
	status, detail := stepResult(st)
	now := time.Now()
	r.setPhases(phaseEdit(r.l.Phases, func(p *wizPhase) {
		if p.Kind != "step" {
			return
		}
		if st.V > 0 {
			p.Label += fmt.Sprintf(" → %.2f V", st.V)
		}
		p.End, p.Status, p.Detail = now.Unix(), status, detail
	}))
}

// resetPhase: a soft reset starts (clock = the saved tune is written while bosminer is stopped).
func (r *levelRun) resetPhase(clock bool) {
	label := "Soft reset"
	if clock {
		label = "Soft reset + saved-tune clock write"
	}
	r.phase("reset", label, cleanMin*time.Minute)
}

// latchPhase: a latch check starts (direct: back to stock from the last step; else first latchHoldMin at the answer).
func (r *levelRun) latchPhase(direct bool) {
	if direct {
		r.phase("latch", "Latch check from the last step", latchMin*time.Minute+20*time.Second)
		return
	}
	r.phase("latch", fmt.Sprintf("Latch check: %d min at the answer, then stock", latchHoldMin),
		(latchHoldMin+latchMin)*time.Minute+20*time.Second)
}

// latchDone: the running latch check ends with its record line.
func (r *levelRun) latchDone(s string) {
	if i := phaseOpen(r.l.Phases); i >= 0 && r.l.Phases[i].Kind == "latch" {
		r.phaseDone(latchStatus(s), s)
	}
}

// endLevel: wizRun's hook after searchLevel: close the timeline with the level's result.
func endLevel(upd func(func(*wizLevel)), res *wizLevel, err error) {
	ps := levelEndPhases(res.Phases, *res, err, time.Now())
	res.Phases = ps
	upd(func(x *wizLevel) { x.Phases = ps })
}
