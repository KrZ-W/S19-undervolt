// core.go — the miner core: HOW the miner runs at a power level, independent of any thermostat.
//
//   - burst-free power change: trim to 0 -> pause -> boards -> power target -> cool -> resume -> settle -> hashing gate
//     -> trim ramp to the level's voltage (the sequence HA's script.s19_set_power_trim ran since 2026-11-22)
//   - trim loop (20 s): hold the level's board voltage with the optocoupler trim (4th-connector GPIO)
//   - fan rule (1 min): chip-temperature steps + jump to the power-matched level on a power raise
//   - chip-temperature cap (1 min): lowers the effective level when chips run hot, raises it back when cool
//
// A thermostat (HA today) only says WHICH level: POST /api/level {"watts": N}. The core runs
// effective = highest table level <= min(requested, cap).
// Each loop has a mode in /etc/s19volt/core.json: "off" | "shadow" (compute + log, no action) | "live".
//
//	GET  /api/core   modes, requested / cap / effective level, change phase, fan + cap state, last decisions
//	POST /api/level  {"watts": N}   (X-Auth) request a level; the core changes when its loops allow
//	POST /api/core   {"fan": "live", ...} (X-Auth) change modes at run time (not persisted unless "save": true)
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type levelEntry struct {
	V  float64 `json:"v"`  // board voltage to hold (0 = stock, no trim)
	D  float64 `json:"d"`  // expected trim drive % for that voltage (ramp target)
	B  string  `json:"b"`  // boards on, e.g. "13"; empty = "123"
	T  int     `json:"t"`  // bosminer power target that gives this level's wall watts when trimmed (0 = the level itself)
	E  float64 `json:"e"`  // estimated J/TH at V (setup wizard; 0 = unknown)
	W  float64 `json:"w"`  // estimated wall watts at V
	S  float64 `json:"s"`  // estimated J/TH at stock voltage (same boards, target T) — the efficiency curve's other line
	SW float64 `json:"sw"` // estimated wall watts at stock voltage
	F  float64 `json:"f"`  // clock factor applied to Braiins' saved tune for T (0/1 = as tuned)
	// level fan (setup wizard): the live fan rule's fan when the level finished; the level's starting fan
	Fan int `json:"fan,omitempty"`
}

// ---- wall-power model (no meter on the miner) ----
// Fitted on a metered S19 (461 steady minutes, trimmed, 3 boards): mean error 8.6 W, 95 % within 24 W.
// bosminer's own figure is wrong when trimmed: it models chip power at the voltage it requested (11.96 V).
const (
	powOverhead = 74.7  // W: fans, control board, PSU losses
	powGain     = 0.988 // per W of bosminer target at the reference voltage
	powExp      = 1.7   // voltage exponent (fitted; less than the textbook 2)
	powRefV     = 11.82 // board voltage at stock
)

// estWall: estimated wall power for bosminer target t at board voltage v (v <= 0 = stock).
func estWall(t int, v float64) float64 { return estWallF(t, v, 1) }

// estWallF: same, with the target's clocks scaled by f (chip power ~ clock).
func estWallF(t int, v, f float64) float64 {
	if v <= 0 {
		v = powRefV
	}
	return powOverhead + powGain*float64(t)*f*math.Pow(v/powRefV, powExp)
}

// targetFor: bosminer target that should give wall watts w at board voltage v (rounded to 5 W).
func targetFor(w, v float64) int {
	if v <= 0 {
		v = powRefV
	}
	t := (w - powOverhead) / (powGain * math.Pow(v/powRefV, powExp))
	return int(math.Round(t/5) * 5)
}

// refVolts: best board voltage per wall watts on the reference miner — the setup wizard's first guess.
var refVolts = [][2]float64{{270, 9.70}, {420, 9.60}, {499, 9.64}, {557, 9.63}, {640, 9.72}, {722, 9.86},
	{834, 10.10}, {912, 10.23}, {1142, 10.61}, {1410, 10.99}, {1757, 11.41}, {1969, 11.54}, {2250, 11.82}}

func refVolt(w float64) float64 {
	if w <= refVolts[0][0] {
		return refVolts[0][1]
	}
	for i := 1; i < len(refVolts); i++ {
		if w <= refVolts[i][0] {
			a, b := refVolts[i-1], refVolts[i]
			return a[1] + (w-a[0])/(b[0]-a[0])*(b[1]-a[1])
		}
	}
	return refVolts[len(refVolts)-1][1]
}

type coreConfig struct {
	Fan     string                `json:"fan"`   // off | shadow | live
	Cap     string                `json:"cap"`   // off | shadow | live
	Trim    string                `json:"trim"`  // off | shadow | live (trim loop)
	Power   string                `json:"power"` // off | live (power changes from /api/level and the cap)
	Mode    string                `json:"mode"`  // manual (the API / this page sets the level; never the voltage) | efficiency (lowest J/TH level) | max (highest level the cooling allows)
	FanMin  int                   `json:"fan_min"`
	FanMax  int                   `json:"fan_max"`
	CapMax  int                   `json:"cap_max"`
	Levels  map[string]levelEntry `json:"levels"`
	TrimCap float64               `json:"trim_cap"`
	MaxAmps float64               `json:"max_amps"` // never trim below watts / max_amps volts (APW12 ~250 A rating; default 200)
}

var (
	ccfg     coreConfig
	ccfgPath string

	coreMu     sync.Mutex
	requested  int
	capW       int
	effective  int
	phase      = "idle"
	phaseSince = time.Now()
	targetV    float64
	decisions  []string

	changeReq = make(chan int, 1)
	fanStart  int // a level's base fan for the fan rule to start from (changeLevel -> fanLoop); guarded by coreMu

	fanSt = fanRule{pct: -1} // the fan rule's state (fanLoop only)
	capSt = struct {
		recent        []float64
		off, up, down time.Time
	}{}
)

func loadCoreConfig(path string) {
	ccfgPath = path
	ccfg = coreConfig{Fan: "shadow", Cap: "shadow", Trim: "shadow", Power: "live", Mode: "manual", FanMin: 15, FanMax: 30, CapMax: 2200, TrimCap: 100, MaxAmps: 200}
	if b, err := os.ReadFile(path); err == nil {
		if err = json.Unmarshal(b, &ccfg); err != nil {
			log.Printf("core: bad %s: %v", path, err)
		}
	} else {
		log.Printf("core: no %s (%v) — defaults, no level table", path, err)
	}
	capW = ccfg.CapMax
}

func note(format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	s := time.Now().Format("15:04:05 ") + msg
	log.Print("core: " + s)
	coreMu.Lock()
	decisions = append(decisions, s)
	if len(decisions) > 100 {
		decisions = decisions[len(decisions)-100:]
	}
	coreMu.Unlock()
	wizEvent("note", "%s", strings.TrimPrefix(msg, "wizard: ")) // the setup monitor's event list (wizard runs only)
}

// minVolts: lowest board voltage allowed at w watts (PSU current limit). Guards against e.g. 3000 W at 7 V.
func minVolts(w int) float64 {
	if ccfg.MaxAmps <= 0 {
		return 0
	}
	return float64(w) / ccfg.MaxAmps
}

func setPhase(p string) {
	coreMu.Lock()
	phase, phaseSince = p, time.Now()
	coreMu.Unlock()
	if p != "idle" && p != "wizard" && p != "change" { // a level change's phases (the wizard's own are stages)
		wizEvent("phase", "level change: %s", p)
	}
}

// ---- table ----

func tableLevels() []int {
	out := []int{}
	for k := range ccfg.Levels {
		if n, err := strconv.Atoi(k); err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

func entryFor(w int) levelEntry {
	e := ccfg.Levels[strconv.Itoa(w)]
	if e.B == "" {
		e.B = "123"
	}
	return e
}

// highest table level <= w (lowest level if none)
func floorLevel(w int) int {
	lv := tableLevels()
	if len(lv) == 0 {
		return w
	}
	best := lv[0]
	for _, l := range lv {
		if l <= w {
			best = l
		}
	}
	return best
}

// ---- readings ----

type reading struct {
	ok      bool
	chip    float64            // hottest chip on enabled boards
	rates   map[string]float64 // TH/s (1 min) per enabled board
	enabled string             // e.g. "13"
	target  int                // bosminer current power target
	mining  bool
	paused  bool
	volts   float64            // mean of enabled boards (> 1 V)
	bvolts  map[string]float64 // per enabled board (> 1 V)
	minRate float64
}

func read() reading {
	r := reading{rates: map[string]float64{}, bvolts: map[string]float64{}}
	raw, err := bos("GET", "/miner/hw/hashboards", nil)
	if err != nil {
		return r
	}
	var hb struct {
		Hashboards []map[string]interface{} `json:"hashboards"`
	}
	if json.Unmarshal(raw, &hb) != nil {
		return r
	}
	r.minRate = math.Inf(1)
	sum := 0.0
	for _, b := range hb.Hashboards {
		id, _ := b["id"].(string)
		if en, _ := b["enabled"].(bool); !en {
			continue
		}
		r.enabled += id
		ths := num(b, "stats", "real_hashrate", "last_1m", "gigahash_per_second") / 1000
		r.rates[id] = ths
		sum += ths
		if ths < r.minRate {
			r.minRate = ths
		}
		if c := num(b, "highest_chip_temp", "temperature", "degree_c"); c > r.chip {
			r.chip = c
		}
	}
	if len(r.rates) == 0 {
		r.minRate = 0
	}
	r.mining = sum > 1
	// The CONFIGURED target (what PUT /performance/power-target sets) comes from /performance/mode. The tuner state's
	// current_target lags: it is null while Braiins tunes from scratch and keeps the OLD target while paused until the
	// new tune starts (found on the first wizard runs 2026-11-25), so it is only a fallback.
	if raw, err = bos("GET", "/performance/mode", nil); err == nil {
		r.target = parseModeTarget(raw)
	}
	if r.target == 0 {
		if raw, err = bos("GET", "/performance/tuner-state", nil); err == nil {
			r.target = int(num(raw, "mode_state", "powertargetmodestate", "current_target", "watt"))
		}
	}
	if v, ok := voltsMap()["boards"].(map[string]float64); ok {
		s, n := 0.0, 0
		for b, x := range v {
			if x > 1 { // a disabled board reads 0 V
				r.bvolts[b] = x
				s += x
				n++
			}
		}
		if n > 0 {
			r.volts = s / float64(n)
		}
	}
	stMu.Lock()
	r.paused = paused
	stMu.Unlock()
	r.ok = true
	return r
}

func trimNow() float64 {
	if drv == nil {
		return 0
	}
	v, _ := drv.Target()
	return v
}

func setTrim(p float64) error {
	if drv == nil {
		return errors.New("no trim driver")
	}
	return drv.Set(p)
}

// trimOffNow drops the drive to 0 at once (no glide): only right before bosminer stops or restarts, so the PWM is
// quiet before bosminer drives the boards' reset lines (they share the GPIO register with the trim pin).
func trimOffNow() {
	if drv != nil {
		drv.SetNow(0)
	}
}

// setup trim pace: every wizard / calibration move glides 5 % per 10 s, both directions
const (
	paceStep  = 5.0
	paceDelay = 10 * time.Second
)

var errAborted = errors.New("aborted")

func plainSleep(d time.Duration) bool { time.Sleep(d); return true }

// rampTrimS: the one trim glide. sleep returning false (a stop request) ends it with wizStopped.
func rampTrimS(p, step float64, delay time.Duration, abort func() bool, sleep func(time.Duration) bool) error {
	for i := 0; i < 200; i++ {
		c := trimNow()
		if math.Abs(c-p) < 0.01 {
			return nil
		}
		if abort != nil && abort() {
			return errAborted
		}
		if err := setTrim(nextTrim(c, p, step)); err != nil {
			return err
		}
		if !sleep(delay) {
			return wizStopped{}
		}
	}
	return errors.New("ramp did not converge")
}

// glide: a setup trim move at the setup pace. Moves DOWN are never interrupted by a stop request (they end at a
// safer voltage); moves up stop when sleep says so. abort (optional) ends the move early.
func glide(p float64, sleep func(time.Duration) bool, abort func() bool) error {
	p = math.Max(0, math.Min(ccfg.TrimCap, p))
	from := trimNow()
	if p < from {
		sleep = plainSleep
	}
	logIt, v0 := math.Abs(from-p) >= 0.01 && wizRunning(), 0.0 // the setup monitor's event list
	if logIt {
		v0 = read().volts
	}
	err := rampTrimS(p, paceStep, paceDelay, abort, sleep)
	if logIt {
		wizEvent("glide", "%s", glideText(from, trimNow(), v0, read().volts, err))
	}
	return err
}

// glideText: one trim glide for the event list, e.g. "trim 0 → 50 %, 11.82 → 11.21 V".
func glideText(from, to, v0, v1 float64, err error) string {
	s := fmt.Sprintf("trim %.0f → %.0f %%, %.2f → %.2f V", from, to, v0, v1)
	if err != nil {
		s += " (ended early: " + err.Error() + ")"
	}
	return s
}

// glideZero: back to stock voltage at the setup pace, whatever happens.
func glideZero() {
	if err := glide(0, plainSleep, nil); err != nil {
		note("glide to 0 %%: %v", err)
	}
}

// ---- power change (one at a time, from the worker) ----

// While a setup run is stopping (stopping()), it returns false within a second: a stop is never held up by a wait
// (a stop during a 30-min Braiins wait kept the run going for over 10 min once). waitUp is the same without the
// stop check, for the short waits a safe exit needs (bosminer back up after a restart).
func waitFor(timeout, every time.Duration, cond func() bool) bool {
	return waitLoop(timeout, every, cond, stopping)
}

func waitUp(timeout, every time.Duration, cond func() bool) bool {
	return waitLoop(timeout, every, cond, func() bool { return false })
}

// waitLoop: cond every `every` until timeout; abort is checked at least every waitSlice (then false).
func waitLoop(timeout, every time.Duration, cond, abort func() bool) bool {
	t0 := time.Now()
	for time.Since(t0) < timeout {
		if abort() {
			return false
		}
		if cond() {
			return true
		}
		for end := time.Now().Add(every); time.Now().Before(end); {
			if abort() {
				return false
			}
			time.Sleep(min(waitSlice, time.Until(end)))
		}
	}
	return false
}

// waitSlice: how often a wait checks for a stop.
const waitSlice = time.Second

// errNotHashing: the hashing gate failed after a change (a board below the expected rate): the miner is at the new
// target at stock voltage. The level table keeps running it; the wizard must recover the boards first.
var errNotHashing = errors.New("a board is not hashing after the change")

func changeLevel(w int) error {
	e := entryFor(w)
	t := e.T
	if t == 0 {
		t = w
	}
	// fans before resume: never below the current fan, the level's base fan or the power-matched fan; the base
	// fan alone only once the level's voltage is reached (gap review 2026-11-25)
	set := -1
	before := func() {
		if ccfg.Fan != "live" {
			return
		}
		cur, _ := fanPct().(int)
		if f := preResumeFan(cur, e.Fan, matchFan(t, ccfg.FanMin, ccfg.FanMax)); f > cur {
			out, err := fanSet(f)
			note("%d W: fan %d -> %d %% before resume: %v %s", w, cur, f, err, out)
			set = f
		} else {
			set = cur
		}
	}
	reached, err := changeTo(t, e, before)
	if err != nil && !errors.Is(err, errNotHashing) {
		return err
	}
	if reached && e.Fan >= minFan && ccfg.Fan == "live" { // at the level's voltage: its base fan from the setup run
		out, ferr := fanSet(e.Fan)
		note("%d W: base fan %d %%: %v %s", w, e.Fan, ferr, out)
		set = e.Fan
	}
	coreMu.Lock()
	effective = w
	if set >= minFan {
		fanStart = set // the fan rule starts from what is set now
	} else if reached && e.Fan >= minFan {
		fanStart = e.Fan // fan mode not live: the rule's own (shadow) state starts from the base fan
	}
	coreMu.Unlock()
	return nil
}

// preResumeFan: the fan to set before resuming after a level change: max(current, base fan, power-matched).
func preResumeFan(cur, base, match int) int {
	f := cur
	if base > f {
		f = base
	}
	if match > f {
		f = match
	}
	return f
}

// matchFan: the power-matched fan for bosminer target w (the fan rule's jump level).
func matchFan(w, min, max int) int {
	return int(math.Min(float64(max), math.Max(float64(min), math.Round(10+15*(math.Max(float64(w), 1000)-1000)/1000))))
}

// changeTo: the burst-free change to power target w with entry e (boards e.B, then trim to e.V; e.V = 0 = stock).
// Used by the level table (changeLevel) and by the setup wizard (any boards, stock voltage). beforeResume (optional)
// runs while paused, right before the resume. Returns whether the entry's voltage was reached (stock entries:
// once hashing), errNotHashing when the hashing gate fails (miner left hashing at stock), or another error.
// An error after our own pause resumes the miner (never left paused by a failed change).
func changeTo(w int, e levelEntry, beforeResume func()) (reached bool, err error) {
	changeStart := time.Now()
	if e.B == "" {
		e.B = "123"
	}
	note("change -> %d W (boards %s, %.2f V)", w, e.B, e.V)
	setPhase("trim to 0")
	coreMu.Lock()
	targetV = 0
	coreMu.Unlock()
	if drv != nil {
		glideZero()
	}
	setPhase("pause")
	if _, err := bos("PUT", "/actions/pause", nil); err != nil {
		return false, fmt.Errorf("pause: %w", err)
	}
	resumed := false
	defer func() {
		if err != nil && !resumed && !errors.Is(err, errNotHashing) {
			if _, rerr := bos("PUT", "/actions/resume", nil); rerr != nil {
				note("%d W: resume after a failed change: %v", w, rerr)
			}
		}
	}()
	waitFor(60*time.Second, 3*time.Second, func() bool { stMu.Lock(); defer stMu.Unlock(); return paused })

	if r := read(); r.ok && r.enabled != e.B {
		setPhase("boards " + r.enabled + " -> " + e.B)
		on, off := []string{}, []string{}
		for _, b := range []string{"1", "2", "3"} {
			if strings.Contains(e.B, b) {
				on = append(on, b)
			} else {
				off = append(off, b)
			}
		}
		if _, err := bos("PATCH", "/miner/hw/hashboards", map[string]interface{}{"enable": true, "hashboard_ids": on}); err != nil {
			return false, fmt.Errorf("boards on: %w", err)
		}
		if len(off) > 0 {
			time.Sleep(5 * time.Second)
			if _, err := bos("PATCH", "/miner/hw/hashboards", map[string]interface{}{"enable": false, "hashboard_ids": off}); err != nil {
				return false, fmt.Errorf("boards off: %w", err)
			}
		}
		waitFor(20*time.Second, 20*time.Second, func() bool { return false }) // bosminer restarts (a setup stop ends it)
	}
	setPhase("power target")
	doneWait := waitingOnBraiins(w, "the power target to be accepted")
	ok := waitFor(3*time.Minute, 10*time.Second, func() bool { // refused for ~1 min after a board restart
		_, err := bos("PUT", "/performance/power-target", map[string]int{"watt": w})
		return err == nil && read().target == w
	})
	doneWait()
	if !ok {
		return false, fmt.Errorf("power target %d not accepted", w)
	}
	setPhase("cool")
	waitFor(3*time.Minute, 10*time.Second, func() bool { r := read(); return r.ok && r.chip <= 55 })
	if beforeResume != nil {
		beforeResume()
	}
	setPhase("resume")
	if _, err := bos("PUT", "/actions/resume", nil); err != nil {
		return false, fmt.Errorf("resume: %w", err)
	}
	resumed = true
	waitFor(3*time.Minute, 5*time.Second, func() bool { stMu.Lock(); defer stMu.Unlock(); return !paused })
	setPhase("settle")
	// Braiins must have FINISHED its tune for this target before any trim: wait for a stage-4 ("S4") round
	// logged after this change started (a fresh tune takes ~12 min, more at high power; a saved one is quick).
	if !waitStage4(w, changeStart, 30*time.Minute) {
		if stopping() { // a setup run was stopped while Braiins tuned: leave now (the deferred resume runs)
			return false, wizStopped{}
		}
		note("%d W: no stage-4 round within 30 min (continuing)", w)
	}
	setPhase("hashing gate")
	gate := float64(w) / 1000 * 2.4
	if !waitFor(5*time.Minute, 10*time.Second, func() bool { r := read(); return r.ok && r.minRate > gate }) {
		note("%d W: a board is below %.1f TH/s after 5 min - staying at stock voltage", w, gate)
		go alert("boards_not_hashing", fmt.Sprintf("at %d W a board is below the expected rate - stock voltage", w))
		return false, errNotHashing
	}
	if e.V > 0 && e.V < minVolts(w) {
		note("%d W: table voltage %.2f V is below the current limit (%.2f V at %.0f A) - staying at stock", w, e.V, minVolts(w), ccfg.MaxAmps)
		return false, nil
	}
	if e.V <= 0 {
		note("at %d W", w)
		return true, nil
	}
	if drv == nil || ccfg.Trim == "off" {
		return false, nil
	}
	setPhase("trim ramp")
	coreMu.Lock()
	targetV = e.V
	coreMu.Unlock()
	if err := glide(e.D, plainSleep, func() bool { r := read(); return r.ok && r.minRate < gate*0.6 }); err != nil {
		note("%d W: trim ramp stopped (%v) - back to stock", w, err)
		coreMu.Lock()
		targetV = 0
		coreMu.Unlock()
		glideZero()
		return false, nil
	}
	note("at %d W", w)
	return true, nil
}

func powerWorker() {
	for w := range changeReq {
		if ccfg.Power != "live" {
			note("power change to %d W skipped (power mode %s)", w, ccfg.Power)
			continue
		}
		if !claimChange() { // under coreMu, same lock as setupBegin: never both
			note("power change to %d W dropped (setup running or core busy)", w)
			continue
		}
		if err := changeLevel(w); err != nil {
			note("change to %d W failed: %v", w, err)
			go alert("power_change_failed", err.Error())
		}
		setPhase("idle")
	}
}

// ask for a change unless one is queued (the newest request wins when the worker frees up)
func requestChange(w int) {
	select {
	case changeReq <- w:
	default:
		select {
		case <-changeReq:
		default:
		}
		changeReq <- w
	}
}

func reconcile() {
	coreMu.Lock()
	req, cp, eff, ph := requested, capW, effective, phase
	coreMu.Unlock()
	if ph != "idle" || setupRunning() || (req == 0 && (ccfg.Mode == "" || ccfg.Mode == "manual")) {
		return
	}
	want := modeLevel(req, cp)
	if want != 0 && want != eff {
		requestChange(want)
	}
}

// modeLevel: the level the current mode wants, never above the cap (0 = nothing to do).
func modeLevel(req, cp int) int {
	lv := tableLevels()
	switch ccfg.Mode {
	case "efficiency":
		best, bestE := 0, math.Inf(1)
		for _, l := range lv {
			if e := ccfg.Levels[strconv.Itoa(l)].E; l <= cp && e > 0 && e < bestE {
				best, bestE = l, e
			}
		}
		if best != 0 {
			return best
		}
	case "max":
		return floorLevel(cp)
	}
	if req == 0 {
		return 0
	}
	return floorLevel(int(math.Min(float64(req), float64(cp))))
}

// ---- loops ----

func idle() bool { coreMu.Lock(); defer coreMu.Unlock(); return phase == "idle" }

func trimLoop() {
	for {
		time.Sleep(20 * time.Second)
		coreMu.Lock()
		tv := targetV
		coreMu.Unlock()
		if ccfg.Trim == "off" || tv <= 0 || !idle() || drv == nil || setupRunning() {
			continue
		}
		r := read()
		if !r.ok || !r.mining || r.volts <= 0 {
			continue
		}
		cur := trimNow()
		gate := float64(r.target) / 1000 * 2.4 * 0.6
		if cur > 0 && r.minRate < gate {
			note("trim: a board fell below 60%% (%.1f TH/s) - trim to stock", r.minRate)
			if ccfg.Trim == "live" {
				coreMu.Lock()
				targetV = 0
				coreMu.Unlock()
				glideZero()
				go alert("board_dropped", "a board fell below 60 % while trimmed - back to stock")
			}
			continue
		}
		if r.volts < minVolts(r.target) && cur > 0 { // over the PSU current limit: back off regardless of the target
			note("trim: %.2f V at %d W is below the current limit %.2f V - backing off", r.volts, r.target, minVolts(r.target))
			if ccfg.Trim == "live" {
				_ = setTrim(math.Max(0, cur-2))
			}
			continue
		}
		err := r.volts - tv
		if math.Abs(err) <= 0.03 {
			continue
		}
		n := math.Round(math.Max(0, math.Min(ccfg.TrimCap, cur+math.Max(-1, math.Min(1, err/0.12))))*100) / 100
		if ccfg.Trim == "live" {
			_ = setTrim(n)
		} else {
			note("trim (shadow): %.2f V vs %.2f -> %.2f %% (now %.2f)", r.volts, tv, n, cur)
		}
	}
}

// fanRule is THE fan rule (one implementation): fanLoop runs it in production and, live, while the setup wizard
// runs (owner decision 2026-11-25: chips run at production-like temperature during the tune, bounded by the
// wizard's maximum fan). pct < 0 = unknown (re-read from the fan).
type fanRule struct {
	pct, lastW        int
	recent            []float64
	off, up, down, pw time.Time
}

// fanLim: the bounds and timings the rule works with. capW < 0 = no cap condition (the wizard: the cap loop
// stands down). up / down = least time between moves up / down (down also waits that long after a move up);
// pwHold = no move down this long after a power jump.
type fanLim struct {
	min, max, capW   int
	up, down, pwHold time.Duration
}

// fan rule timings: production (unchanged) and the setup wizard's faster set (owner 2026-11-25: the fan must
// settle within a level). Thresholds (74 / 73 rising / 70 °C) are the same for both.
var (
	fanTimesCore = fanLim{up: 2 * time.Minute, down: 3 * time.Minute, pwHold: 10 * time.Minute}
	fanTimesWiz  = fanLim{up: time.Minute, down: time.Minute, pwHold: 3 * time.Minute}
)

// step: one minute of the rule — chips at 74 °C (or 73 and rising) -> +5 % (lim.up apart, up to max); three cool
// readings (<= 70 °C, not rising, cap not binding) -> -2 % (lim.down after any move, lim.pwHold after a power jump);
// off for 10 min -> min; back on or a +300 W jump -> the power-matched fan. Returns the new fan %.
func (f *fanRule) step(mining bool, chip float64, target int, lim fanLim, t time.Time) int {
	match := matchFan(target, lim.min, lim.max)
	jump := f.lastW > 0 && target-f.lastW >= 300
	if f.lastW == 0 {
		f.lastW = target
	}
	p, np := f.pct, f.pct
	if mining {
		f.recent = append(f.recent, chip)
		if len(f.recent) > 3 {
			f.recent = f.recent[len(f.recent)-3:]
		}
	} else {
		f.recent = nil
	}
	wasOff := !f.off.IsZero()
	rec := f.recent
	capOK := lim.capW < 0 || lim.capW >= target
	switch {
	case !mining:
		if f.off.IsZero() {
			f.off = t
		}
		if t.Sub(f.off) >= 10*time.Minute {
			np = lim.min
		}
	case wasOff || (jump && p < match):
		np = match
	case (chip >= 74 || (chip >= 73 && len(rec) >= 2 && rec[len(rec)-1]-rec[len(rec)-2] >= 1)) && p < lim.max && t.Sub(f.up) >= lim.up:
		np = int(math.Min(float64(lim.max), float64(p+5)))
	case len(rec) == 3 && maxf(rec) <= 70 && rec[2] <= rec[0] && capOK && p > lim.min &&
		t.Sub(f.down) >= lim.down && t.Sub(f.up) >= lim.down && t.Sub(f.pw) >= lim.pwHold:
		np = int(math.Max(float64(lim.min), float64(p-2)))
	}
	if mining {
		f.off = time.Time{}
	}
	if jump {
		f.pw = t
	}
	f.lastW = target
	if np > p {
		f.up = t
	} else if np < p {
		f.down = t
	}
	f.pct = np
	return np
}

// fanRulePct: the fan the rule last set or read (-1 = unknown); read by the wizard to re-apply after restarts.
var fanRulePct atomic.Int32

func init() { fanRulePct.Store(-1) }

// fanNow: the rule's current fan, else def.
func fanNow(def int) int {
	if p := int(fanRulePct.Load()); p >= minFan {
		return p
	}
	return def
}

// fanOwner: who the rule works for now. The setup wizard: live, minFan..its maximum fan, no cap condition. A
// calibration (setup without the wizard): the rule stands down. Otherwise core.json (fan off = stands down).
func fanOwner() (who string, lim fanLim, act, live bool) {
	if wizRunning() {
		l := fanTimesWiz
		l.min, l.max, l.capW = minFan, wizMaxFan(), -1
		return "wizard", l, true, true
	}
	if setupRunning() {
		return "setup", fanLim{}, false, false
	}
	if ccfg.Fan == "off" {
		return "off", fanLim{}, false, false
	}
	l := fanTimesCore
	l.min, l.max, l.capW = ccfg.FanMin, ccfg.FanMax, capW
	return "core", l, true, ccfg.Fan == "live"
}

func fanLoop() {
	owner := ""
	for {
		time.Sleep(time.Minute)
		who, lim, act, live := fanOwner()
		if who != owner { // new owner: re-read the fan, start fresh
			fanSt.pct, fanSt.recent, owner = -1, nil, who
		}
		if !act {
			continue
		}
		r := read()
		if !r.ok {
			continue
		}
		coreMu.Lock()
		if fanStart >= minFan && r.mining { // a level change (or the wizard) set a fan: start from it (no
			fanSt.pct, fanSt.up, fanStart = fanStart, time.Now(), 0 // power-matched jump, no "was off" reset)
			fanSt.off, fanSt.lastW = time.Time{}, 0
		}
		coreMu.Unlock()
		if fanSt.pct < 0 {
			if p, ok := fanPct().(int); ok {
				fanSt.pct = p
			} else {
				fanSt.pct = 20
			}
		}
		p := fanSt.pct
		np := fanSt.step(r.mining, r.chip, r.target, lim, time.Now())
		fanRulePct.Store(int32(np))
		// the real fan is read back every tick: bosminer can move it behind the rule's back (a restart starts at its
		// configured/minimum speed; a write during start-up is not confirmed) - 2026-11-25 it sat at 25 % while the
		// rule believed 15 % for a whole level.
		drift := false
		if live && r.mining && np == p {
			if a := fanPWM(); a >= 0 && fanDrifted(a, np) {
				drift = true
				note("fan (%s): fan at %d %%, rule says %d %% - setting it again", who, a, np)
			}
		}
		if np != p || drift {
			if live && r.mining {
				out, err := fanSet(np)
				note("fan (%s) %d -> %d %% (chip %.1f, %d W): %v %s", who, p, np, r.chip, r.target, err, out)
			} else {
				note("fan (%s, %s) %d -> %d %% (chip %.1f, %d W)", who, ccfg.Fan, p, np, r.chip, r.target)
			}
		}
	}
}

// fanDrifted: the real fan is more than 2 % away from the rule's (bosminer's own 2-s bursts at 80 °C are
// shorter than a tick and rarely seen; a re-set then is harmless).
func fanDrifted(actual, rule int) bool { return actual > rule+2 || actual < rule-2 }

// fanRuleStart: the rule continues from fan p (set by the caller) at its next tick.
func fanRuleStart(p int) {
	coreMu.Lock()
	fanStart = p
	coreMu.Unlock()
	fanRulePct.Store(int32(p))
}

func capLoop() {
	for {
		time.Sleep(time.Minute)
		if ccfg.Cap == "off" || setupRunning() {
			continue
		}
		r := read()
		if !r.ok {
			continue
		}
		t := time.Now()
		coreMu.Lock()
		c := capW
		coreMu.Unlock()
		minL := 360
		if lv := tableLevels(); len(lv) > 0 {
			minL = lv[0]
		}
		if r.mining {
			capSt.recent = append(capSt.recent, r.chip)
			if len(capSt.recent) > 5 {
				capSt.recent = capSt.recent[len(capSt.recent)-5:]
			}
			capSt.off = time.Time{}
		} else {
			capSt.recent = nil
			if capSt.off.IsZero() {
				capSt.off = t
			}
		}
		rec := capSt.recent
		nc := c
		switch {
		case !r.mining:
			if t.Sub(capSt.off) >= 10*time.Minute {
				nc = ccfg.CapMax
			}
		case (r.chip >= 80 || (r.chip >= 78.5 && len(rec) >= 2 && rec[len(rec)-1]-rec[len(rec)-2] >= 1)) && t.Sub(capSt.down) >= 7*time.Minute:
			nc = int(math.Max(float64(minL), math.Min(float64(c), float64(r.target))-100))
		case len(rec) == 5 && maxf(rec) <= 77.5 && rec[4] <= rec[0] && maxf(rec)+2.32 <= 79 && c < ccfg.CapMax &&
			t.Sub(capSt.up) >= 15*time.Minute && t.Sub(capSt.down) >= 15*time.Minute:
			nc = c + 100
		}
		if nc < c {
			capSt.down = t
		} else if nc > c {
			capSt.up = t
		}
		if nc != c {
			note("cap (%s) %d -> %d W (chip %.1f, running %d W)", ccfg.Cap, c, nc, r.chip, r.target)
			if ccfg.Cap == "live" {
				coreMu.Lock()
				capW = nc
				coreMu.Unlock()
				reconcile()
			}
		}
	}
}

func maxf(a []float64) float64 {
	m := math.Inf(-1)
	for _, x := range a {
		m = math.Max(m, x)
	}
	return m
}

// ---- HTTP ----

func authOK(r *http.Request) bool {
	return wcfg.UIPassword == "" || r.Header.Get("X-Auth") == wcfg.UIPassword // no password set = open
}

func handleCore(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if !authOK(r) {
			w.WriteHeader(401)
			jsonOut(w, map[string]interface{}{"ok": false, "error": "wrong or missing password"})
			return
		}
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if m, ok := req["mode"].(string); ok && (m == "manual" || m == "efficiency" || m == "max") {
			ccfg.Mode = m
			note("mining mode = %s", m)
			go reconcile()
		}
		for k, p := range map[string]*string{"fan": &ccfg.Fan, "cap": &ccfg.Cap, "trim": &ccfg.Trim, "power": &ccfg.Power} {
			if v, ok := req[k].(string); ok && (v == "off" || v == "shadow" || v == "live") {
				*p = v
				note("mode %s = %s", k, v)
			}
		}
		if s, _ := req["save"].(bool); s {
			_ = saveCoreConfig("")
		}
	}
	coreMu.Lock()
	defer coreMu.Unlock()
	jsonOut(w, map[string]interface{}{"mode": ccfg.Mode, "modes": map[string]string{"fan": ccfg.Fan, "cap": ccfg.Cap, "trim": ccfg.Trim, "power": ccfg.Power},
		"requested": requested, "cap_w": capW, "effective": effective, "phase": phase,
		"phase_s": int(time.Since(phaseSince).Seconds()), "target_v": targetV, "fan_pct": fanSt.pct,
		"levels": tableLevels(), "table": ccfg.Levels, "decisions": decisions})
}

func handleLevel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !authOK(r) {
		w.WriteHeader(401)
		jsonOut(w, map[string]interface{}{"ok": false, "error": "POST with X-Auth"})
		return
	}
	noteAPICall(r)
	if refuseDuringSetup(w) {
		return
	}
	var req struct {
		Watts int `json:"watts"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Watts < 300 || req.Watts > 3000 {
		w.WriteHeader(400)
		jsonOut(w, map[string]interface{}{"ok": false, "error": "watts 300..3000"})
		return
	}
	coreMu.Lock()
	requested = req.Watts
	coreMu.Unlock()
	note("requested %d W", req.Watts)
	reconcile()
	jsonOut(w, map[string]interface{}{"ok": true, "effective_target": floorLevel(int(math.Min(float64(req.Watts), float64(capW))))})
}

// saveCoreConfig writes the core config (caller holds no lock); backup = also keep a timestamped copy first.
func saveCoreConfig(backup string) error {
	if backup != "" {
		if old, err := os.ReadFile(ccfgPath); err == nil {
			_ = os.WriteFile(fmt.Sprintf("%s.%s-%d", ccfgPath, backup, time.Now().Unix()), old, 0600)
		}
	}
	coreMu.Lock()
	b, err := json.MarshalIndent(ccfg, "", " ")
	coreMu.Unlock()
	if err != nil {
		return err
	}
	return os.WriteFile(ccfgPath, b, 0600)
}

func setupCore(path string) {
	loadCoreConfig(path)
	wizLoad()
	go restoreAfterRestart() // stopped during the fixed test profile: Braiins' tuner back
	go fanGuard()            // fans held at their speed through every bosminer stop/start (fanhold.go)
	loadChips()
	if r := read(); r.ok {
		effective = r.target // whatever bosminer runs now; trim loop starts only after the core's own change
	}
	http.HandleFunc("/api/core", handleCore)
	http.HandleFunc("/api/level", handleLevel)
	http.HandleFunc("/api/calibrate", handleCalibrate)
	http.HandleFunc("/api/wizard", handleWizard)
	http.HandleFunc("/api/wizard/chips", handleWizardChips)
	go powerWorker()
	go trimLoop()
	go fanLoop()
	go capLoop()
	note("core up: fan %s, cap %s, trim %s, power %s, %d levels, running %d W", ccfg.Fan, ccfg.Cap, ccfg.Trim, ccfg.Power, len(ccfg.Levels), effective)
}

// parseModeTarget: the configured power target from GET /performance/mode
// ({"tunermode":{"target":{"powertarget":{"power_target":{"watt":2200}}}}}, verified on BOS+ 26.11).
func parseModeTarget(raw json.RawMessage) int {
	return int(num(raw, "tunermode", "target", "powertarget", "power_target", "watt"))
}
