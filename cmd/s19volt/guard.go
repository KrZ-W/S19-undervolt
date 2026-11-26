package main

// guard.go — guards around the setup runs:
//
//   - run guard: while the wizard or a calibration sweep runs, every outside control (/api/control, /api/level,
//     /fan?pct=, /boost, the wizard's listen test) is refused with 409 {"error":"setup running"}, and the core's
//     own loops (chip cap, trim loop, mode reconcile, power worker) stand down. The fan rule stands down during a
//     calibration only; during the wizard it drives the fans live (core.go fanOwner: 15 %..the wizard's maximum,
//     faster timings). The setup itself calls the internal functions, never the HTTP handlers.
//   - bosminer events: the log watcher tracks stage-4 rounds, "Setting voltage" and real retunes (tuneTracker); a
//     trimmed hold that saw either is thrown out and redone (limits in touchVerdict).
//   - Braiins' minimum power target, read from bosminer's constraints when the API offers it (else 360 W).

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- run guard ----

var setupWho string // "" | "wizard" | "calibrate"; guarded by coreMu (set together with the phase)

var setupActive atomic.Bool // = setupWho != "", readable without coreMu (waitFor's stop check)

// stopping: a setup run is active and its stop was requested (a stale flag outside a run does not count).
func stopping() bool { return setupActive.Load() && stopFlag.Load() }

// setupBegin claims the core for a setup run when it is idle; false if busy. Clears a stale stop request.
func setupBegin(who, ph string) (bool, string) {
	coreMu.Lock()
	defer coreMu.Unlock()
	if phase != "idle" || setupWho != "" {
		busy := phase
		if setupWho != "" {
			busy = setupWho
		}
		return false, busy
	}
	phase, phaseSince, setupWho = ph, time.Now(), who
	stopFlag.Store(false)
	setupActive.Store(true)
	return true, ""
}

// setupEnd gives the core back: phase idle and no setup owner, in one step (the power worker claims under the
// same lock, so it never sees a half-ended setup).
func setupEnd() {
	coreMu.Lock()
	phase, phaseSince, setupWho = "idle", time.Now(), ""
	setupActive.Store(false)
	coreMu.Unlock()
}

// claimChange: the power worker claims the core for one level change, under the same lock as setupBegin, so a
// level change and a setup run can never both start. false = a setup runs or another change is in progress.
func claimChange() bool {
	coreMu.Lock()
	defer coreMu.Unlock()
	if setupWho != "" || phase != "idle" {
		return false
	}
	phase, phaseSince = "change", time.Now()
	return true
}

// ---- stop request (wizard and calibration): one shared flag, checked by every wait of a setup run ----

var stopFlag atomic.Bool

func requestStop()        { stopFlag.Store(true) }
func stopRequested() bool { return stopFlag.Load() }

// stoppableSleep waits d; false (early) once a stop was requested. The flag stays set until the next setupBegin,
// so every later wait of the same run also ends at once.
func stoppableSleep(d time.Duration) bool {
	end := time.Now().Add(d)
	for {
		if stopFlag.Load() {
			return false
		}
		rem := time.Until(end)
		if rem <= 0 {
			return true
		}
		if rem > 250*time.Millisecond {
			rem = 250 * time.Millisecond
		}
		time.Sleep(rem)
	}
}

func setupRunning() bool { coreMu.Lock(); defer coreMu.Unlock(); return setupWho != "" }

// refuseDuringSetup answers 409 while a setup run owns the miner; true = refused (the handler returns).
func refuseDuringSetup(w http.ResponseWriter) bool {
	if !setupRunning() {
		return false
	}
	w.WriteHeader(http.StatusConflict)
	jsonOut(w, map[string]interface{}{"ok": false, "error": "setup running"})
	return true
}

// ---- what bosminer does to the voltage and the tune (verified on the 26.11 nightly log, 2026-11-25) ----
//
// Stage 4 is bosminer's steady state: a round every ~8.5 min ("Tune/all: S4 itN: target W ..." ends round N, then
// "Stage 4: Profile tuned at ...", "itN+1: ramping to V", "Status: measuring performance ..."). None of those lines
// is a retune. A round DOES move the PSU voltage when bosminer's own power estimate is off target
// ("PSU: Setting voltage A -> B", a few small steps); after a round or two it settles (a round with no
// "Setting voltage"). So:
//   - settled = a stage-4 round with no "Setting voltage" since the previous round (same target): wait for it
//     before trimming a level;
//   - a "Setting voltage" during a trimmed hold moved the board voltage under us: the step is redone (not a retune);
//   - a real retune is "Entering STAGE 1|2|3", "profile failed", a bosminer (re)start (or HALT / Disable voltage).
//     "Entering STAGE 4: stable" is printed when a saved tune loads with a power-target change: not a retune.

type lineKind int

const (
	lineOther      lineKind = iota
	lineSetVoltage          // "PSU: Setting voltage": bosminer moved the PSU voltage
	lineRound               // "Tune/all: S4 itN: target W": a stage-4 round finished (W = its target)
	lineRetune              // a real retune or a restart
)

// The one parser of bosminer's tuner lines (real examples, 26.11 nightly):
//
//	Tune/all: Using strategy s3: Entering STAGE 2: Finding threshold voltage
//	Tune/all: S3 it33: target 1000.00 W, measured n/a, estimated 828.58 W, 11.960 V | hb1 261.2 MHz mcr 99.9% | ...
var (
	roundRe   = regexp.MustCompile(`Tune/all: S([0-9]+) it([0-9]+): target ([0-9.]+) W(?:, measured ([^,]+), estimated ([0-9.]+) W, ([0-9.]+) V)?`)
	hbRe      = regexp.MustCompile(`hb([0-9]+) ([0-9.]+) MHz mcr ([0-9.]+)%`)
	setVoltRe = regexp.MustCompile(`PSU: Setting voltage`)
	stageRe   = regexp.MustCompile(`Entering STAGE ([0-9]+)(?:: *([^\r\n]*))?`)
	restartRe = regexp.MustCompile(`Starting BOSminer`)
	retuneRe  = regexp.MustCompile(`[Pp]rofile failed|Starting BOSminer|HALT|Disable voltage`)
)

// tunerBoard / tunerRound: one tuning round as bosminer logs it ("S3 it33": stage 3, round 33).
type tunerBoard struct {
	Board string  `json:"board"`
	MHz   float64 `json:"mhz"`
	MCR   float64 `json:"mcr"` // %
}

type tunerRound struct {
	Stage    int          `json:"stage"`
	It       int          `json:"round"`
	Target   int          `json:"target_w"`
	Measured string       `json:"measured,omitempty"` // bosminer's own power reading ("n/a" without a meter)
	EstW     float64      `json:"est_w"`
	V        float64      `json:"v"`
	Boards   []tunerBoard `json:"boards,omitempty"`
}

// parseRound: a "Tune/all: S<stage> it<round>: target ..." line.
func parseRound(line string) (tunerRound, bool) {
	m := roundRe.FindStringSubmatch(line)
	if m == nil {
		return tunerRound{}, false
	}
	r := tunerRound{Measured: strings.TrimSpace(m[4])}
	r.Stage, _ = strconv.Atoi(m[1])
	r.It, _ = strconv.Atoi(m[2])
	t, _ := strconv.ParseFloat(m[3], 64)
	r.Target = int(math.Round(t))
	r.EstW, _ = strconv.ParseFloat(m[5], 64)
	r.V, _ = strconv.ParseFloat(m[6], 64)
	for _, b := range hbRe.FindAllStringSubmatch(line, -1) {
		mhz, _ := strconv.ParseFloat(b[2], 64)
		mcr, _ := strconv.ParseFloat(b[3], 64)
		r.Boards = append(r.Boards, tunerBoard{Board: b[1], MHz: mhz, MCR: mcr})
	}
	return r, true
}

// parseStage: an "Entering STAGE N: name" line.
func parseStage(line string) (int, string, bool) {
	m := stageRe.FindStringSubmatch(line)
	if m == nil {
		return 0, "", false
	}
	n, _ := strconv.Atoi(m[1])
	return n, strings.TrimSpace(m[2]), true
}

// classifyLine: what one bosminer log line means for the guards (target only for lineRound).
func classifyLine(line string) (lineKind, int) {
	if r, ok := parseRound(line); ok {
		if r.Stage == 4 {
			return lineRound, r.Target
		}
		return lineOther, 0 // a round of the tune itself (stages 1-3): bosminer is still tuning
	}
	if setVoltRe.MatchString(line) {
		return lineSetVoltage, 0
	}
	if n, _, ok := parseStage(line); ok && n != 4 {
		return lineRetune, 0
	}
	if retuneRe.MatchString(line) {
		return lineRetune, 0
	}
	return lineOther, 0
}

// tuneTracker: bosminer's voltage / tune events, fed by the log watcher. The one source for /state, the stage-4
// waits and the profile guard.
type tuneTracker struct {
	lastSet, lastRound, lastSettled, lastRetune time.Time
	roundTarget, settledTarget                  int
	setLine, retuneLine                         string
	// the live tuner state for the monitor (any stage)
	stage            int
	stageName        string
	stageAt, tunedAt time.Time
	tuned            *tunerRound // the latest round line, any stage
	restartAt        time.Time
	// what a setup run or a level change waits for ("" = nothing)
	wait   string
	waitW  int
	waitAt time.Time
}

// feed takes one log line; returns a short event text for a tuner stage change or a bosminer start ("" = none).
func (k *tuneTracker) feed(line string, ts time.Time) string {
	ev := ""
	if r, ok := parseRound(line); ok {
		k.tuned, k.tunedAt = &r, ts
	} else if n, name, ok := parseStage(line); ok {
		k.stage, k.stageName, k.stageAt = n, name, ts
		ev = fmt.Sprintf("Braiins: stage %d", n)
		if name != "" {
			ev += " (" + name + ")"
		}
	} else if restartRe.MatchString(line) {
		k.stage, k.stageName, k.stageAt, k.tuned, k.tunedAt, k.restartAt = 0, "", time.Time{}, nil, time.Time{}, ts
		ev = "bosminer started"
	}
	kind, w := classifyLine(line)
	switch kind {
	case lineSetVoltage:
		k.lastSet, k.setLine = ts, shortLine(line)
	case lineRetune:
		k.lastRetune, k.retuneLine = ts, shortLine(line)
		k.lastRound = time.Time{} // the next round has no clean predecessor
	case lineRound:
		if !k.lastRound.IsZero() && k.roundTarget == w && !k.lastSet.After(k.lastRound) {
			k.lastSettled, k.settledTarget = ts, w
		}
		k.lastRound, k.roundTarget = ts, w
	}
	return ev
}

// tunerView: Braiins' live tuner state as the API shows it (ages in s, -1 = never).
type tunerView struct {
	Stage      int         `json:"stage"`
	StageName  string      `json:"stage_name,omitempty"`
	StageAge   int         `json:"stage_age_s"`
	Round      *tunerRound `json:"round,omitempty"`
	RoundAge   int         `json:"round_age_s"`
	RestartAge int         `json:"restart_age_s"`
	Wait       string      `json:"wait,omitempty"`          // what the setup / a level change waits for
	WaitTarget int         `json:"wait_target_w,omitempty"` // ... at this power target
	WaitAge    int         `json:"wait_age_s"`
	Line       string      `json:"line"` // one-line summary (tunerLine)
}

func ageS(t, now time.Time) int {
	if t.IsZero() {
		return -1
	}
	return int(now.Sub(t).Seconds())
}

func (k *tuneTracker) view(now time.Time) tunerView {
	v := tunerView{Stage: k.stage, StageName: k.stageName, StageAge: ageS(k.stageAt, now), RoundAge: ageS(k.tunedAt, now),
		RestartAge: ageS(k.restartAt, now), Wait: k.wait, WaitTarget: k.waitW, WaitAge: ageS(k.waitAt, now)}
	if k.tuned != nil {
		r := *k.tuned
		r.Boards = append([]tunerBoard(nil), r.Boards...)
		v.Round = &r
	}
	v.Line = tunerLine(v)
	return v
}

// tunerHead: "Braiins tuning 1000 W from scratch" (stages other than 4) / "Braiins at 1000 W" (stage 4).
func tunerHead(stage, w int) string {
	tw := ""
	if w > 0 {
		tw = fmt.Sprintf(" %d W", w)
	}
	if stage == 4 {
		if tw == "" {
			return "Braiins stable"
		}
		return "Braiins at" + tw
	}
	return "Braiins tuning" + tw + " from scratch"
}

// roundText: "stage 3 round 33 (est. 829 W, 11.96 V, mcr 98–100 %)".
func roundText(r *tunerRound) string {
	s := fmt.Sprintf("stage %d round %d (", r.Stage, r.It)
	if r.Measured != "" && r.Measured != "n/a" {
		s += "measured " + r.Measured + ", "
	}
	s += fmt.Sprintf("est. %.0f W, %.2f V", r.EstW, r.V)
	if len(r.Boards) > 0 {
		lo, hi := 1000.0, -1.0
		for _, b := range r.Boards {
			lo, hi = math.Min(lo, b.MCR), math.Max(hi, b.MCR)
		}
		if math.Round(lo) == math.Round(hi) {
			s += fmt.Sprintf(", mcr %.0f %%", hi)
		} else {
			s += fmt.Sprintf(", mcr %.0f–%.0f %%", lo, hi)
		}
	}
	return s + ")"
}

// tunerLine: the one-line "what is Braiins doing" summary, with what we wait for.
func tunerLine(v tunerView) string {
	w, r := v.WaitTarget, v.Round
	match := r != nil && (w == 0 || r.Target == w)
	if w == 0 && r != nil {
		w = r.Target
	}
	var s string
	switch {
	case match && v.Stage != 0 && v.Stage != r.Stage && v.StageAge >= 0 && v.StageAge < v.RoundAge:
		s = fmt.Sprintf("%s: stage %d%s, after %s", tunerHead(v.Stage, w), v.Stage, paren(v.StageName), roundText(r))
	case match:
		s = tunerHead(r.Stage, w) + ": " + roundText(r)
	case v.Stage != 0:
		s = fmt.Sprintf("%s: stage %d%s, no tuning round yet", tunerHead(v.Stage, w), v.Stage, paren(v.StageName))
	case v.RestartAge >= 0:
		s = fmt.Sprintf("bosminer started %s ago, Braiins not tuning yet", agoText(v.RestartAge))
	default:
		s = "Braiins: no tuner line in the log yet"
	}
	last := v.RoundAge
	if v.StageAge >= 0 && (last < 0 || v.StageAge < last) {
		last = v.StageAge
	}
	if (match || v.Stage != 0) && last > 15*60 {
		s += fmt.Sprintf(", last tuner line %s ago", agoText(last))
	}
	if v.Wait != "" {
		s += " — waiting for " + v.Wait
		if v.WaitAge >= 60 {
			s += fmt.Sprintf(" (%s so far)", agoText(v.WaitAge))
		}
	}
	return s
}

func paren(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

func agoText(s int) string {
	if s < 90 {
		return fmt.Sprintf("%d s", s)
	}
	return fmt.Sprintf("%d min", (s+30)/60)
}

// settledSince: a settled stage-4 round for target w after t.
func (k *tuneTracker) settledSince(w int, t time.Time) bool {
	return k.settledTarget == w && k.lastSettled.After(t)
}

// stage4Since: Braiins reached stage 4 after t (the stage line, or a stage-4 round for target w).
func (k *tuneTracker) stage4Since(w int, t time.Time) bool {
	return (k.stage == 4 && k.stageAt.After(t)) || (k.roundTarget == w && k.lastRound.After(t))
}

// since: the last "Setting voltage" and the last retune line after t ("" = none).
func (k *tuneTracker) since(t time.Time) (set, retune string) {
	if k.lastSet.After(t) {
		set = k.setLine
	}
	if k.lastRetune.After(t) {
		retune = k.retuneLine
	}
	return
}

func shortLine(l string) string {
	l = strings.TrimSpace(l)
	if len(l) > 160 {
		l = l[:160]
	}
	return l
}

var tune tuneTracker // guarded by stMu (the log watcher)

// noteLine is called by the log watcher (stMu held); returns a tuner event text ("" = none).
func noteLine(line string, ts time.Time) string { return tune.feed(line, ts) }

// tunerNow: the live tuner state + summary line (for /api/wizard, /api/status and the wizard's events).
func tunerNow() tunerView {
	stMu.Lock()
	defer stMu.Unlock()
	return tune.view(time.Now())
}

// waitingOnBraiins marks a wait for Braiins (shown on the monitor with the live tuner state); call the returned
// func when the wait ends.
func waitingOnBraiins(w int, what string) func() {
	stMu.Lock()
	tune.wait, tune.waitW, tune.waitAt = what, w, time.Now()
	stMu.Unlock()
	wizEvent("wait", "waiting for Braiins at %d W: %s", w, what)
	return func() {
		stMu.Lock()
		if tune.wait == what && tune.waitW == w {
			tune.wait, tune.waitW, tune.waitAt = "", 0, time.Time{}
		}
		v := tune.view(time.Now())
		stMu.Unlock()
		wizEvent("wait", "done waiting (%s): %s", what, v.Line)
	}
}

// bosEventsSince: the last "Setting voltage" and the last retune line logged after t ("" = none).
func bosEventsSince(t time.Time) (set, retune string) {
	stMu.Lock()
	defer stMu.Unlock()
	return tune.since(t)
}

// waitStage4: a stage-4 round for target w logged after `since` and no "Setting voltage" for 30 s (bosminer has
// finished its tune: a fresh one takes ~12 min, more at high power; a saved one is quick).
func waitStage4(w int, since time.Time, timeout time.Duration) bool {
	defer waitingOnBraiins(w, "stage 4")()
	return waitFor(timeout, 15*time.Second, func() bool {
		stMu.Lock()
		defer stMu.Unlock()
		return tune.roundTarget == w && tune.lastRound.After(since) && time.Since(tune.lastSet) > 30*time.Second
	})
}

// settleTimeout: how long the wizard waits for a settled stage-4 round before it proceeds anyway (and notes it).
const settleTimeout = 40 * time.Minute

// waitSettled: a settled stage-4 round for w after since (false on timeout or stop).
func waitSettled(w int, since time.Time, timeout time.Duration) bool {
	defer waitingOnBraiins(w, "a settled stage-4 round (no voltage change for a whole round)")()
	t0 := time.Now()
	for time.Since(t0) < timeout {
		stMu.Lock()
		ok := tune.settledSince(w, since)
		stMu.Unlock()
		if ok {
			return true
		}
		if !wizSleep(15 * time.Second) {
			return false
		}
	}
	return false
}

// maxRedos: steps thrown out per level (any cause) before the level fails.
const maxRedos = 4

// touchVerdict: after a thrown-out step (redos / retunes = how many this level already had), "" = redo the step,
// else why the level fails. A real retune gets one redo; "Setting voltage" (stage-4 settling) has no own limit;
// both count toward maxRedos.
func touchVerdict(retune bool, redos, retunes int) string {
	switch {
	case retune && retunes >= 1:
		return "bosminer retuned during trimmed holds twice"
	case redos >= maxRedos:
		return fmt.Sprintf("%d steps thrown out (bosminer moved the voltage during holds)", redos)
	}
	return ""
}

// ---- Braiins' minimum power target ----

const fallbackMinTarget = 360 // W; ~359 W seen on the reference miner

var (
	minTgtMu  sync.Mutex
	minTgtW   int
	minTgtSrc string
)

// parseMinTarget reads the power-target minimum from a bosminer constraints reply (REST JSON, snake or camel case).
func parseMinTarget(raw json.RawMessage) int {
	for _, p := range [][]string{
		{"tuner_constraints", "power_target", "min", "watt"},
		{"tunerConstraints", "powerTarget", "min", "watt"},
	} {
		if w := num(raw, p...); w > 0 {
			return int(w + 0.5)
		}
	}
	return 0
}

// minPowerTarget: Braiins' own minimum power target (bosminer's constraints endpoint, cached once read),
// else the fallback. Also returns where it came from.
func minPowerTarget() (int, string) {
	minTgtMu.Lock()
	defer minTgtMu.Unlock()
	if minTgtW > 0 {
		return minTgtW, minTgtSrc
	}
	if raw, err := bos("GET", "/configuration/constraints", nil); err == nil {
		if w := parseMinTarget(raw); w > 0 {
			minTgtW, minTgtSrc = w, "bosminer constraints"
			return minTgtW, minTgtSrc
		}
	}
	return fallbackMinTarget, "fallback (bosminer constraints not readable)" // not cached: retried next time
}

// ---- leaving a setup run (gap review 2026-11-25): never trimmed, never paused, the core takes over ----

// safeStop: trim back to 0 % at the setup pace, then the miner resumed (bosminer given time to answer first).
// Every setup exit runs it: done, error, stop, panic.
func safeStop() {
	coreMu.Lock()
	targetV = 0
	coreMu.Unlock()
	glideZero()
	if !waitFor(4*time.Minute, 10*time.Second, func() bool { return read().ok }) {
		note("setup exit: bosminer does not answer - resume may fail")
	}
	if _, err := bos("PUT", "/actions/resume", nil); err != nil {
		note("setup exit: resume: %v", err)
	}
}

// exitLevel: the level to run after a setup: what the mode / request wants, else the level from before the run.
func exitLevel(want, pre int) int {
	if want > 0 {
		return want
	}
	return pre
}

// handBack ends a wizard run after safeStop: effective = 0 (whatever the wizard left is not a table level, so
// reconcile acts), fan = max(current, power-matched for the level to come), the setup ended, then that level
// re-requested (power mode live) or the pre-run power target put back.
func handBack(preW, preTarget int) {
	coreMu.Lock()
	effective = 0
	req, cp := requested, capW
	coreMu.Unlock()
	w := exitLevel(modeLevel(req, cp), preW)
	t := w
	if e, ok := ccfg.Levels[strconv.Itoa(w)]; ok && e.T > 0 {
		t = e.T
	}
	if t > 0 {
		cur, _ := fanPct().(int)
		if f := preResumeFan(cur, 0, matchFan(t, ccfg.FanMin, ccfg.FanMax)); f > cur {
			out, err := fanSet(f)
			note("setup exit: fan %d -> %d %% (power-matched for %d W): %v %s", cur, f, t, err, out)
		}
	}
	setupEnd()
	switch {
	case w <= 0:
		note("setup exit: no level to go back to")
	case ccfg.Power == "live":
		note("setup exit: requesting %d W", w)
		requestChange(w)
	case preTarget > 0:
		if _, err := bos("PUT", "/performance/power-target", map[string]int{"watt": preTarget}); err != nil {
			note("setup exit: putting back the power target %d W: %v", preTarget, err)
		} else {
			note("setup exit: power target %d W put back (power mode %s)", preTarget, ccfg.Power)
		}
	}
}

// recoverSetup: a panic in a setup goroutine is logged, then the trim glides to 0 and the miner resumes.
func recoverSetup(who string) {
	if p := recover(); p != nil {
		note("%s: PANIC %v - trim to 0 %%, resume", who, p)
		go alert("setup_panic", fmt.Sprintf("%s crashed (%v): trim back to 0 %%, miner resumed", who, p))
		safeStop()
	}
}
