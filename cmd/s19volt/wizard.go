package main

// wizard.go — the setup wizard: from a freshly wired miner to a level table, unattended.
//
// The user chooses a wattage range + step, a maximum fan level (noise ceiling; the minimum is fixed
// at 15 %) and a target chip temperature. The wizard then runs, in order:
//
//	check    the miner, the trim output and the voltage readings
//	fixed    the conservative fixed test profile (fixedmode.go; Braiins' tuner off)
//	sweep    trim 0 -> 100 % in sweepStep steps on that profile (board voltage after a short settle each). It is
//	         also the trim test: >= 0.15 V below the 0 % reading proves the mod works (shown at once); no such drop
//	         by 60 % fails the stage (check the wiring). Then the drive chart, startDrive's curve, and the
//	         series-resistor floor = the mean voltage at 100 % (expected ~8.8 V with 33 kΩ). Boards that stop
//	         hashing do NOT end it (flagged per point, information only)
//	droop    manual mode at the user's min and max wattage, 3 drive points each (droop.go): the PSU's load droop,
//	         fitted into the drive model's load term before the first level
//	tuner    Braiins' tuner back (bosminer restarts: clears anything the sweep latched), saved tunes moved aside
//	search   per level: soft reset (pause, cool, bosminer off with trim 0, on) -> fresh Braiins tune = the
//	         baseline (a sliding 5-min window measured during the settled-round wait) -> DRIVE steps in gallop + bisection order (search.go; a step = a drive, its voltage = the
//	         hold's mean, no corrections; each step judged as before, a soft reset after every failing step) ->
//	         answer = the passing step with the best est. J/TH -> latch check of the answer (latchAnswer: from the last step if it
//	         ran at or below the answer's voltage, else 5 min at the answer first; latched: soft reset,
//	         +0.05 V as drive, measure again) -> clocks scaled to land on the watts
//	table    a level table to review and apply
//
// Efficiency only (owner decision 2026-11-25): the answer is the passing step with the best est. J/TH (answerStep); the strict answer is gone.
// Fans: the core's fan rule (core.go fanRule) drives them LIVE during the whole run, between 15 % and the user's
// maximum, so chips run at production-like temperature; a level's fan = the rule's fan when the level finishes.
// While a setup runs, outside control is refused and the core's other loops stand down (guard.go).
// Every trim move under load glides 5 % per 2 s (core.go glide); a soft reset sets trim 0 directly at the moment
// bosminer cuts the board power. A board below 60 % of its baseline rate ends a descent or aborts a hold.
// Without a power meter, efficiency is ESTIMATED as bosminer's power target × (V / V_stock)² — dynamic
// chip power scales with V². The estimate ranks voltages and board counts; it is not a wall reading.
//
// Not built: resume. An interruption (stop, reboot, power loss) means starting again; results of
// finished stages stay visible in /etc/s19volt/wizard.json.
//
//	GET  /api/wizard                  state (monitor page)
//	POST /api/wizard {"action":"start", inputs...} | {"action":"stop"} | {"action":"apply","trim_live":true}
//	POST /api/wizard {"action":"listen","fan":30}   fans to that level for 30 s, then back

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	minFan    = 15   // fixed: below this, problems appear even when the temperature reading looks fine
	holdMin   = 5    // minutes of the stock baseline window and of a full hold (search parameters: search.go)
	latchMin  = 3    // minutes of the latch window back at stock (owner 2026-11-25: 5 -> 3; the verdict now uses exact Total MH window averages and per-chip averaged data, the old "3 too noisy" was about single smoothed readings)
	rankFr    = 0.97 // board ranking: a board "holds" a reading while it keeps this share of its stock rate
	cleanOff  = 20 * time.Second
	twoBoardW = 600 // fewer-boards option: 2 boards tested below this level
	oneBoardW = 400 // ... and 1 board below this one
)

type wizInputs struct {
	MinW        int     `json:"min_w"`
	MaxW        int     `json:"max_w"`
	StepW       int     `json:"step_w"`
	MaxFan      int     `json:"max_fan"`
	ChipTarget  float64 `json:"chip_target"`
	FewerBoards bool    `json:"fewer_boards"`
	ResetTunes  bool    `json:"reset_tunes"` // acknowledged: saved Braiins tunes are moved to a backup first (required)
	MarginV     float64 `json:"margin_v"`    // added to the answer's voltage in the table (default 0)
}

type wizStage struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	Status  string `json:"status"` // waiting | running | done | failed | skipped
	Detail  string `json:"detail"`
	Started int64  `json:"started"`
	Ended   int64  `json:"ended"`
	EstMin  int    `json:"est_min"`
}

type wizCurve struct {
	Watts  int        `json:"watts"`
	StockV float64    `json:"stock_v"`
	Points []calPoint `json:"points"`
	Status string     `json:"status"`
}

// drivePt: one measured (load, board volts, drive %) point. Load = bosminer power target x clock factor (the
// same key as a curve's Watts). Src: "step" (a measured search step), "wiring" or "floor" (the sweep's last point),
// "droop" (droop.go: W = estimated wall watts of the user's min / max, T = its load key).
type drivePt struct {
	W   int     `json:"w"`
	T   int     `json:"t,omitempty"` // droop points: the load key (bosminer-target equivalent); W is wall watts
	V   float64 `json:"v"`
	D   float64 `json:"d"`
	B   string  `json:"b"`
	Src string  `json:"src"`
	L   int     `json:"l"` // level index (steps only; -1 otherwise)
	// Short: a short check (not a full hold): less weight in the drive model's fit (drivemodel.go)
	Short bool `json:"short,omitempty"`
}

// wizStep: one measured voltage step. Old wizard.json files (strict fields) still load; unknown fields are ignored.
// The search runs in drive space: Drive is the step (set), V the hold's mean board voltage (measured; 0 = unknown,
// e.g. an aborted hold), BV the same mean per board.
type wizStep struct {
	V       float64            `json:"v"`
	Drive   float64            `json:"drive"`
	BV      map[string]float64 `json:"bv,omitempty"`
	THs     float64            `json:"ths"`
	JTH     float64            `json:"jth"`      // estimated J/TH (power model / measured TH/s)
	Share   float64            `json:"share"`    // slowest board vs its stock rate
	Shares  map[string]float64 `json:"shares"`   // per board vs its stock rate (board ranking)
	NewWeak int                `json:"new_weak"` // chips weak at this step but not at stock (step rule: <= 1)
	Weak    []string           `json:"weak_new,omitempty"`
	MCR     float64            `json:"mcr,omitempty"`     // step mcr: the hold's per-chip hashrate vs the stock baseline (0 = unknown)
	MCRs    map[string]float64 `json:"mcrs,omitempty"`    // ... per board
	MCRTH   float64            `json:"mcr_ths,omitempty"` // ... as TH/s (share counters only)
	Chip    float64            `json:"chip"`
	Fan     int                `json:"fan,omitempty"`     // fan % (the live fan rule's) at the end of the step
	Pass    bool               `json:"pass"`              // passed the step rule (stepPass)
	Stable  bool               `json:"stable"`            // = Pass (chip map marks)
	Full    bool               `json:"full"`              // a full holdMin hold (else a short check)
	Dropped string             `json:"dropped,omitempty"` // board below 60 % of its stock rate: hold aborted
	Invalid string             `json:"invalid,omitempty"` // thrown out: bosminer touched the voltage/tune (log line)
}

type wizLevel struct {
	Watts   int       `json:"watts"`  // level = estimated wall watts (round number)
	Boards  string    `json:"boards"` // "123", or "n2"/"n1" until the ranking resolves them
	Target  int       `json:"target"` // bosminer power target that lands on Watts at V
	EstW    float64   `json:"est_w"`  // estimated wall watts at the result
	Status  string    `json:"status"` // waiting | running | done | unsafe (no safe answer: stock) | too_hot | failed | skipped
	StockV  float64   `json:"stock_v"`
	StockTH float64   `json:"stock_ths"`
	ChipC   float64   `json:"chip_c"`
	V       float64   `json:"v"`     // chosen voltage: the answer step's mean board voltage (+ margin; 0 = stock)
	Drive   float64   `json:"drive"` // the answer step's drive % (the search runs in drive; margin = less drive at the local slope)
	THs     float64   `json:"ths"`
	EstJTH  float64   `json:"est_jth"`   // estimated J/TH at V
	StockJT float64   `json:"stock_jth"` // estimated J/TH at stock
	StockW  float64   `json:"stock_w"`   // estimated wall watts at stock (target t0)
	Steps   []wizStep `json:"steps"`
	Note    string    `json:"note"`
	Chosen  bool      `json:"chosen"` // best board count for this level
	Minutes float64   `json:"minutes"`
	Index   int       `json:"index"` // position in the run (chip map key)
	Clock   float64   `json:"clock"` // clock factor applied to Braiins' tune for Target (1 = as tuned)
	// baseline = the fresh Braiins tune after the clean start
	StockRates map[string]float64 `json:"stock_rates,omitempty"` // per-board TH/s
	StockWeak  []string           `json:"stock_weak,omitempty"`  // chips already weak in the fresh tune
	// the search (search.go): prediction, the answer, the latch check
	Pred       float64    `json:"pred,omitempty"`       // predicted best voltage (the first step)
	Resets     int        `json:"resets,omitempty"`     // soft resets after failing steps
	Raises     int        `json:"raises,omitempty"`     // +latchRaise after latches
	LatchedAt  []float64  `json:"latched_at,omitempty"` // answers that latched
	Eff        *wizAnswer `json:"eff,omitempty"`
	Answer     string     `json:"answer,omitempty"`     // efficiency | stock
	Latched    []string   `json:"latched,omitempty"`    // newly weak chips still weak back at stock voltage (last check)
	LatchKnown bool       `json:"latch_known"`          // per-chip data was available for the latch check
	LatchPath  string     `json:"latch_path,omitempty"` // how the latch check(s) ran (latchAnswer), e.g. "latch check from the last step (10.31 V avg, ...): clean -> answer cleared"
	ChipSrc    string     `json:"chip_src,omitempty"`   // per-chip data source: shares (patched bosminer) | hashrate (stock, 30-s mean)
	RateSrc    string     `json:"rate_src,omitempty"`   // stock TH/s source: devs (Total MH window average) | boards (API 1-min mean)
	// guards
	MinTarget      int        `json:"min_target,omitempty"` // Braiins' minimum: the target was raised to it
	Redos          int        `json:"redos,omitempty"`      // steps thrown out and redone (bosminer moved the voltage or retuned)
	Retunes        int        `json:"retunes,omitempty"`    // ... of which real retunes (one allowed per level)
	ProfileChanged bool       `json:"profile_changed"`      // Braiins' saved tune for Target changed during the level
	ProfileNote    string     `json:"profile_note,omitempty"`
	Fan            int        `json:"fan,omitempty"`     // the live fan rule's fan at the end of the level (the level's default fan)
	Phases         []wizPhase `json:"phases,omitempty"`  // the level's timeline (timeline.go)
	Check          string     `json:"check,omitempty"`   // the final cross-level check's note (resume.go finalCheck)
	Outside        bool       `json:"outside,omitempty"` // finished, kept, but outside the current range (extend.go)
}

type wizState struct {
	Running  bool       `json:"running"`
	Inputs   wizInputs  `json:"inputs"`
	Stages   []wizStage `json:"stages"`
	Floor    float64    `json:"floor_v"`
	WiringDV float64    `json:"wiring_dv"`     // old runs (separate 50 % trim test stage); kept for display
	Mod      *wizMod    `json:"mod,omitempty"` // the sweep's mod check: the first point >= modDV below stock
	Curves   []wizCurve `json:"curves"`
	Points   []drivePt  `json:"drive_points,omitempty"` // measured drive points (starting drive for later moves)
	Levels   []wizLevel `json:"levels"`
	Ranking  []string   `json:"ranking"` // boards strongest -> weakest at low voltage
	Message  string     `json:"message"`
	Started  int64      `json:"started"`
	Ended    int64      `json:"ended"`
	Applied  int64      `json:"applied"`
	Remain   int        `json:"remaining_min"`
	Live     string     `json:"live"`
	Events   []wizEv    `json:"events,omitempty"` // the last maxWizEvents, oldest first
	// fixed test profile (fixedmode.go): what the early stages ran on; Fallback = why they ran on a tuned level
	Fixed *wizFixed `json:"fixed,omitempty"`
	// target to put back in tuner mode while our manual mode may be on (0 = not in manual mode)
	ManualRestore int `json:"manual_restore_w,omitempty"`
	// resume (resume.go): the miner the preparation ran on, where this run moved Braiins' saved tunes (never moved
	// twice), the resume times, the final cross-level check's line
	Miner       *wizMiner `json:"miner,omitempty"`
	TunesBackup string    `json:"tunes_backup,omitempty"`
	Resumed     []int64   `json:"resumed,omitempty"`
	FinalCheck  string    `json:"final_check,omitempty"`
}

// wizMod: where the trim sweep proved the mod acts on the voltage.
type wizMod struct {
	StockV float64 `json:"stock_v"` // the 0 % reading
	V      float64 `json:"v"`
	Duty   float64 `json:"duty"`
}

// wizFixed: the fixed manual profile of a run's early stages.
type wizFixed struct {
	MHz      float64 `json:"mhz"`
	Volt     float64 `json:"volt"`               // requested = Braiins' minimum PSU voltage
	VoltSrc  string  `json:"volt_src,omitempty"` // bosminer constraints or the fallback
	Target   int     `json:"target"`             // bosminer target that clock corresponds to (gates)
	Fallback string  `json:"fallback,omitempty"` // manual mode failed: the old tuned path was used
}

// wizEv: one timestamped event of a run (stage start/end, level changes and their phases, Braiins stage changes,
// glides, holds, latch checks, clean applies, redos, errors). T = unix ms (the page shows local time).
type wizEv struct {
	T    int64  `json:"t"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

const maxWizEvents = 30

// addWizEv appends e, keeping the newest maxWizEvents.
func addWizEv(evs []wizEv, e wizEv) []wizEv {
	evs = append(evs, e)
	if len(evs) > maxWizEvents {
		evs = append([]wizEv(nil), evs[len(evs)-maxWizEvents:]...)
	}
	return evs
}

// wizEvent records an event while the wizard runs (no-op otherwise). Never called with wizMu held. Saved with the
// next wizSet.
func wizEvent(kind, format string, a ...interface{}) {
	wizMu.Lock()
	defer wizMu.Unlock()
	if !wiz.Running {
		return
	}
	wiz.Events = addWizEv(wiz.Events, wizEv{T: time.Now().UnixMilli(), Kind: kind, Text: fmt.Sprintf(format, a...)})
}

func wizRunning() bool { wizMu.Lock(); defer wizMu.Unlock(); return wiz.Running }

// wizMaxFan: the running wizard's maximum fan (the live fan rule's upper bound).
func wizMaxFan() int { wizMu.Lock(); defer wizMu.Unlock(); return wiz.Inputs.MaxFan }

var (
	wizMu sync.Mutex
	wiz   wizState
)

func wizSet(f func(s *wizState)) {
	wizMu.Lock()
	f(&wiz)
	wizMu.Unlock()
	wizSave()
}

func wizSnap() wizState {
	wizMu.Lock()
	defer wizMu.Unlock()
	b, _ := json.Marshal(wiz)
	var s wizState
	_ = json.Unmarshal(b, &s)
	return s
}

func wizSave() {
	s := wizSnap()
	if b, err := json.MarshalIndent(s, "", " "); err == nil {
		_ = os.WriteFile(wizFile()+".tmp", b, 0o644)
		_ = os.Rename(wizFile()+".tmp", wizFile())
	}
}

func wizLoad() {
	if b, err := os.ReadFile(wizFile()); err == nil {
		var s wizState
		if json.Unmarshal(b, &s) == nil {
			if s.Running { // a run in progress when s19volt stopped: interrupted (resumable, resume.go)
				s.Message = "interrupted (s19volt restarted)"
				s.Live, s.Remain = "", 0
			}
			s.Running = false
			for i := range s.Stages {
				if s.Stages[i].Status == "running" {
					s.Stages[i].Status, s.Stages[i].Detail = "failed", "interrupted (s19volt restarted)"
				}
			}
			for i := range s.Levels {
				if s.Levels[i].Status == "running" {
					s.Levels[i].Status, s.Levels[i].Note = "interrupted", "interrupted (s19volt restarted)"
					s.Levels[i].Phases = phaseClose(s.Levels[i].Phases, "fail", "interrupted (s19volt restarted)", time.Now())
				}
			}
			wizMu.Lock()
			wiz = s
			wizMu.Unlock()
		}
	}
}

// wizSleep waits d; false if a stop was requested (shared stop flag, guard.go).
func wizSleep(d time.Duration) bool { return stoppableSleep(d) }

type wizStopped struct{}

func (wizStopped) Error() string { return "stopped" }

func wizLive(format string, a ...interface{}) {
	wizSet(func(s *wizState) { s.Live = fmt.Sprintf(format, a...) })
}

// ---- levels and estimates ----

func (in wizInputs) levels() []int {
	out := []int{}
	for w := in.MinW; w <= in.MaxW; w += in.StepW {
		out = append(out, w)
	}
	if len(out) > 0 && out[len(out)-1] != in.MaxW {
		out = append(out, in.MaxW)
	}
	return out
}

type wizJob struct {
	W int
	B string // "123" or "n2"/"n1" (n strongest boards, resolved after the ranking)
}

// jobs, low to high power. The first job is always 3 boards at the bottom of the range: it ranks the boards.
// With the fewer-boards option, levels below the range are added for 2 boards (< 600 W) and 1 board (< 400 W),
// and in-range levels below those thresholds are also tried with fewer boards.
func (in wizInputs) jobs() []wizJob {
	lv := in.levels()
	out := []wizJob{{lv[0], "123"}}
	if in.FewerBoards {
		for w := in.MinW - in.StepW; w >= 300; w -= in.StepW {
			lv = append([]int{w}, lv...)
		}
	}
	for _, w := range lv {
		if !(w == in.MinW) && w >= in.MinW {
			out = append(out, wizJob{w, "123"})
		}
		if in.FewerBoards && w < twoBoardW {
			out = append(out, wizJob{w, "n2"})
		}
		if in.FewerBoards && w < oneBoardW {
			out = append(out, wizJob{w, "n1"})
		}
	}
	sort.SliceStable(out[1:], func(i, j int) bool { return out[1+i].W < out[1+j].W })
	return out
}

// levelMin: typical minutes per searched level: soft reset 4 + Braiins' fresh tune and settled round ~20 (the stock
// baseline is measured during it) + our own search (searchOurMin; first = the first level of a board set, which
// gallops with the larger step).
// levelMaxMin: the worst case (settle wait cap + a full baseline window after it, every step a full hold with a
// reset, clock landing, every raise).
const (
	cleanMin     = 4
	braiinsMin   = 20
	clockLandMin = 30
)

func levelMin(first bool) int { return cleanMin + braiinsMin + searchOurMin(first) }

func levelMaxMin() int {
	return cleanMin + 12 + int(settleTimeout.Minutes()) + holdMin + maxSteps*(1+holdMin+cleanMin) + cleanMin + latchHoldMin + latchMin + 1 +
		maxRaises*(cleanMin+1+holdMin+latchMin+1) + clockLandMin
}

// firstJobs: how many jobs are the first level of their board set (the ranking job, then one per fewer-board set).
func firstJobs(jobs []wizJob) int {
	seen := map[string]bool{}
	for _, j := range jobs {
		seen[j.B] = true
	}
	return len(seen)
}

// sweepStep / sweepSettle / sweepReads: the trim sweep's step (%), the wait after a step before reading (the trim
// driver glides 0.5 s, the capacitor settles in ~3 s), then sweepReads board-voltage reads 1.5 s apart (~12 s)
// averaged: single reads carried the ±0.3 V reading noise and stale values (the 2026-11-25 curve was jagged).
// About 20 s per point.
const (
	sweepStep   = 2.0
	sweepSettle = 8.0
	sweepReads  = 9
)

// The sweep's mod check (it replaces the separate 50 % trim test): a drop of modDV below the 0 % reading proves
// the mod works; none by modBy % fails the stage.
const (
	modDV = 0.15
	modBy = 60.0
)

// sweepMin: the trim sweep (51 points x ~3.5 s ~= 3 min) plus the glide back from 100 % (200 s).
const sweepMin = 7

// midW: the middle of the range rounded down to the step (the fallback path's curve level).
func midW(in wizInputs) int { return in.MinW + ((in.MaxW-in.MinW)/2/in.StepW)*in.StepW }

// estMinutes: the estimate shown before a run. check = API checks; fixed = manual mode on + clocks at the profile +
// PSU settled; sweep = the trim sweep (it includes the trim test) + glide back (5 %/10 s: 100 -> 0 % takes 200 s);
// droop = the load droop (droop.go: two manual-mode loads, 3 drive points each); tuner = tuner mode back + saved tunes moved aside (bosminer restart). The fallback (manual mode refused) adds a
// fresh Braiins tune (~30 min); not counted. Old wizard.json files with "wiring"/"floor"/"curves" stages still load.
func estMinutes(in wizInputs) map[string]int {
	return map[string]int{"check": 3, "fixed": fixedModeMin, "sweep": sweepMin, "droop": droopMin,
		"tuner": tunerBackMin, "search": searchMinutes(in.jobs()), "table": 0}
}

// searchMinutes: the search stage's typical minutes for these jobs.
func searchMinutes(jobs []wizJob) int {
	f := firstJobs(jobs)
	return f*levelMin(true) + (len(jobs)-f)*levelMin(false)
}

// ---- Braiins saved tunes ----

var autotuneFile = "/etc/bosminer-autotune.json" // var: tests point it at a copy

// moveBraiinsTunes moves bosminer's saved tunes to a timestamped backup so every level is tuned fresh.
// bosminer rewrites the file from memory while it runs, so: stop -> move -> start. Never deleted.
func moveBraiinsTunes() (string, error) {
	if _, err := os.Stat(autotuneFile); err != nil {
		return "no saved Braiins tunes", nil
	}
	glideZero() // bosminer stop/start below: never with the trim on
	_ = os.MkdirAll("/etc/s19volt/backup", 0o755)
	bk := fmt.Sprintf("/etc/s19volt/backup/bosminer-autotune-%s.json", time.Now().Format("2006-01-02-150405"))
	_ = exec.Command("/etc/init.d/bosminer", "stop").Run() // prints an error on Braiins but stops it
	waitUp(90*time.Second, 3*time.Second, func() bool { return exec.Command("pidof", "bosminer").Run() != nil })
	if err := exec.Command("cp", "-p", autotuneFile, bk).Run(); err != nil {
		_ = exec.Command("/etc/init.d/bosminer", "start").Run()
		return "", fmt.Errorf("backup of the saved tunes failed: %v", err)
	}
	if err := os.Remove(autotuneFile); err != nil {
		_ = exec.Command("/etc/init.d/bosminer", "start").Run()
		return "", err
	}
	_ = exec.Command("/etc/init.d/bosminer", "start").Run()
	if !waitUp(4*time.Minute, 10*time.Second, func() bool { return read().ok }) {
		return bk, fmt.Errorf("bosminer did not come back after moving the tunes (backup: %s)", bk)
	}
	note("wizard: saved Braiins tunes moved to %s", bk)
	return bk, nil
}

// ---- measurements ----

// avgRates samples the boards for d, returning per-board mean TH/s, total, hottest chip and mean voltage.
// It also measures every chip's work over the same window into lastChips (weak chips, chip map; metrics.go
// chipRates: chip_shares deltas when a patched bosminer publishes them, else the stock chip_hashrate gauge averaged
// over the window). The TH/s are the exact window average from bosminer's Total MH counters (devs.go) when the
// cgminer API answers.
var lastChips map[string][]float64
var lastFreqs map[string][]float64 // chip clocks (MHz) at the end of the last avgRates window
var lastChipSrc string             // "shares" | "hashrate" | "" (no per-chip data)
var lastDevs [2]devsSnap           // the devs reads at the start and the end of the last avgRates window
var lastRateSrc string             // "devs" (Total MH) | "boards" (API 1-min mean): the last window's TH/s

// errBoardDrop: a board fell below 60 % of its stock rate during a hold (the hold is aborted, the step failed).
type errBoardDrop struct {
	board      string
	rate, base float64
}

func (e errBoardDrop) Error() string {
	return fmt.Sprintf("board %s at %.1f TH/s, below 60 %% of its stock %.1f TH/s", e.board, e.rate, e.base)
}

// dropCheck: an errBoardDrop if a board of ref (stock rates; nil = no check) is below 60 % in this reading.
func dropCheck(r reading, ref map[string]float64) error {
	if ref == nil || !r.ok {
		return nil
	}
	if b := boardDropped(r.rates, ref, dropFr); b != "" {
		return errBoardDrop{b, r.rates[b], ref[b]}
	}
	return nil
}

// measWin: one measurement window's raw data: the board readings (15 s), the metrics scrapes at its ends, the
// chip_hashrate samples in it (~30 s), the devs reads at its ends, and its length.
type measWin struct {
	rs      []reading
	m0, m1  chipMetrics
	hr      []map[string][]float64
	d0, d1  devsSnap
	minutes float64
}

// summarize: the one summary of a window (avgRates, the sliding stock baseline): per-board TH/s and total (Total MH
// window average, else the board rates), hottest chip, mean voltage; per-chip work into lastChips / lastChipSrc /
// lastFreqs, mean board voltages into lastBVolts. errCounterReset = bosminer restarted under the window (the other
// values are still set, from the board rates).
func summarize(w measWin) (map[string]float64, float64, float64, float64, error) {
	rates, tot, chip, v, err := windowRates(w.rs)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	lastBVolts = meanBoardVolts(w.rs)
	windowChips(w, rates)
	lastDevs = [2]devsSnap{w.d0, w.d1}
	rates, tot, lastRateSrc, err = exactRates(w.d0, w.d1, rates, tot)
	return rates, tot, chip, v, err
}

// avgRates: ref = stock per-board rates for the 60 % abort (nil during the latch window).
func avgRates(d time.Duration, ref map[string]float64) (map[string]float64, float64, float64, float64, error) {
	w := measWin{m0: scrapeChips(), d0: readDevs()}
	t0 := time.Now()
	end := time.Now().Add(d)
	for i := 0; time.Now().Before(end); i++ {
		if i > 0 && i%2 == 0 { // chip_hashrate every 30 s
			if m := scrapeChips(); m.ok && len(m.hrate) > 0 {
				w.hr = append(w.hr, m.hrate)
			}
		}
		r := read()
		if err := dropCheck(r, ref); err != nil {
			return nil, 0, 0, 0, err
		}
		w.rs = append(w.rs, r)
		if !wizSleep(15 * time.Second) {
			return nil, 0, 0, 0, wizStopped{}
		}
	}
	w.d1, w.m1 = readDevs(), scrapeChips()
	if w.m1.ok && len(w.m1.hrate) > 0 {
		w.hr = append(w.hr, w.m1.hrate)
	}
	w.minutes = time.Since(t0).Minutes()
	return summarize(w)
}

// counterReset: err is a window invalidated by a bosminer restart (errCounterReset).
func counterReset(err error) bool {
	_, ok := err.(errCounterReset)
	return ok
}

// lastBVolts: the per-board mean voltage of the last avgRates window (like lastChips: read right after the call).
var lastBVolts map[string]float64

// meanBoardVolts: per-board mean board voltage over a window's readings (every reading while mining, each board's own
// count: the readings refresh per board every ~10 s, staggered). nil = none.
func meanBoardVolts(rs []reading) map[string]float64 {
	sum, n := map[string]float64{}, map[string]int{}
	for _, r := range rs {
		if !r.ok || !r.mining {
			continue
		}
		for b, v := range r.bvolts {
			if v > 0 {
				sum[b] += v
				n[b]++
			}
		}
	}
	if len(sum) == 0 {
		return nil
	}
	for b := range sum {
		sum[b] /= float64(n[b])
	}
	return sum
}

// mixVolts: two per-board means weighted wa / wb (a short check extended to the full hold); a board missing on one
// side keeps the other side's mean. Rounded to mV.
func mixVolts(a, b map[string]float64, wa, wb float64) map[string]float64 {
	if a == nil && b == nil {
		return nil
	}
	out := map[string]float64{}
	for k, va := range a {
		if vb, ok := b[k]; ok {
			out[k] = va*wa + vb*wb
		} else {
			out[k] = va
		}
	}
	for k, vb := range b {
		if _, ok := a[k]; !ok {
			out[k] = vb
		}
	}
	for k := range out {
		out[k] = math.Round(out[k]*1000) / 1000
	}
	return out
}

// windowRates: one measurement window's readings -> per-board mean TH/s, total, hottest chip, mean voltage
// (readings while not mining are skipped). The board-rate part of summarize (and its fallback TH/s).
func windowRates(rs []reading) (map[string]float64, float64, float64, float64, error) {
	sum := map[string]float64{}
	n, chip, vs, vn := 0, 0.0, 0.0, 0
	for _, r := range rs {
		if !r.ok || !r.mining {
			continue
		}
		for b, t := range r.rates {
			sum[b] += t
		}
		n++
		chip = math.Max(chip, r.chip)
		if r.volts > 0 {
			vs += r.volts
			vn++
		}
	}
	if n == 0 {
		return nil, 0, 0, 0, fmt.Errorf("no readings")
	}
	tot := 0.0
	for b := range sum {
		sum[b] /= float64(n)
		tot += sum[b]
	}
	v := 0.0
	if vn > 0 {
		v = vs / float64(vn)
	}
	return sum, tot, chip, v, nil
}

// scrapeChips: one metrics scrape, parsed at once (only the per-chip parts are kept; failed = !ok).
func scrapeChips() chipMetrics {
	m, err := fetchMetrics()
	if err != nil {
		m = ""
	}
	return parseChipMetrics(m)
}

// windowChips: every chip's work over window w (chipRates) into lastChips / lastChipSrc, the clocks of its last
// scrape into lastFreqs; that scrape failed -> no per-chip data. Boards not in on are dropped.
func windowChips(w measWin, on map[string]float64) {
	lastChips, lastChipSrc = nil, ""
	if !w.m1.ok {
		return
	}
	lastChips, lastChipSrc = chipRates(w.m0, w.m1, w.minutes, w.hr)
	lastFreqs = w.m1.freqs
	for b := range lastChips {
		if _, ok := on[b]; !ok {
			delete(lastChips, b) // disabled boards
		}
	}
}

// ---- the stock baseline: a sliding window during the settled-round wait (owner decision 2026-11-25) ----

// rateSample: one 15-s reading of the baseline window with the parsed per-chip metrics at that moment (a few KB;
// the raw metrics text is never kept).
type rateSample struct {
	at time.Time
	r  reading
	m  chipMetrics
	dv devsSnap
}

// baseWindow: the stock baseline's rolling buffer. restart discards it (a "Setting voltage" or a retune line);
// add keeps only what the last `hold` needs (the newest sample at least hold old, and everything after it).
type baseWindow struct {
	start time.Time // clean since: collection start or the last discard
	s     []rateSample
}

func (w *baseWindow) restart(t time.Time) { w.start, w.s = t, nil }

// anchor: index of the newest sample at least hold before the last one (-1 = not yet hold of clean samples).
func (w *baseWindow) anchor(hold time.Duration) int {
	if len(w.s) == 0 {
		return -1
	}
	end := w.s[len(w.s)-1].at
	a := -1
	for i, x := range w.s {
		if !x.at.After(end.Add(-hold)) {
			a = i
		}
	}
	return a
}

func (w *baseWindow) add(x rateSample, hold time.Duration) {
	w.s = append(w.s, x)
	if a := w.anchor(hold); a > 0 {
		w.s = append([]rateSample(nil), w.s[a:]...)
	}
}

// collected: clean time in the buffer, capped at hold (for the live line).
func (w *baseWindow) collected(hold time.Duration) time.Duration {
	if len(w.s) < 2 {
		return 0
	}
	d := w.s[len(w.s)-1].at.Sub(w.s[0].at)
	if d > hold {
		d = hold
	}
	return d
}

// last: the last hold of the buffer as one window (summarize): the readings after the anchor sample, the metrics
// and devs reads of the anchor and the newest sample, the chip_hashrate samples after the anchor. ok = false until
// hold is covered.
func (w *baseWindow) last(hold time.Duration) (measWin, bool) {
	a := w.anchor(hold)
	if a < 0 || a == len(w.s)-1 {
		return measWin{}, false
	}
	var mw measWin
	for _, x := range w.s[a+1:] {
		mw.rs = append(mw.rs, x.r)
		if x.m.ok && len(x.m.hrate) > 0 {
			mw.hr = append(mw.hr, x.m.hrate)
		}
	}
	e := w.s[len(w.s)-1]
	mw.m0, mw.m1, mw.d0, mw.d1 = w.s[a].m, e.m, w.s[a].dv, e.dv
	mw.minutes = e.at.Sub(w.s[a].at).Minutes()
	return mw, true
}

// ---- chip map: per-chip mcr at every step, kept apart from the state (it is large) ----

type chipSnap struct {
	Label  string               `json:"label"` // "stock", "10.80 V", "re-target 10.40 V"
	V      float64              `json:"v"`
	Stable bool                 `json:"stable"`
	Chips  map[string][]float64 `json:"chips"` // board -> 76 chips' valid shares per minute during the hold (-1 = unknown)
}

var (
	chipMu   sync.Mutex
	wizChips = map[int][]chipSnap{} // level index -> snapshots in order
)

// wizard files live next to core.json (/etc/s19volt on the miner)
func wizFile() string  { return filepath.Join(filepath.Dir(ccfgPath), "wizard.json") }
func chipFile() string { return filepath.Join(filepath.Dir(ccfgPath), "wizard-chips.json") }

func addChipSnap(level int, label string, v float64, stable bool) {
	if lastChips == nil {
		return
	}
	chipMu.Lock()
	wizChips[level] = append(wizChips[level], chipSnap{Label: label, V: v, Stable: stable, Chips: lastChips})
	b, _ := json.Marshal(wizChips)
	chipMu.Unlock()
	_ = os.WriteFile(chipFile(), b, 0o644)
}

func loadChips() {
	if b, err := os.ReadFile(chipFile()); err == nil {
		chipMu.Lock()
		_ = json.Unmarshal(b, &wizChips)
		chipMu.Unlock()
	}
}

// recoverBoards clears latched chips without a smart plug: a soft reset (cleanApply: pause -> cool -> bosminer
// stop with trim 0 -> off time -> start -> fan -> resume) -> verify every enabled board. A second cycle uses a
// longer off time; if boards are still down, the caller stops and asks for a power cycle.
func recoverBoards(fan, gateTarget int) error {
	for i, off := range []time.Duration{20 * time.Second, 60 * time.Second} {
		note("wizard: recovering latched chips (cycle %d, %s off)", i+1, off)
		if err := cleanApply(fan, gateTarget, off, nil); err == nil {
			return nil
		} else if _, stopped := err.(wizStopped); stopped {
			return err
		}
	}
	return errPowerCycle
}

// cleanApply is the one soft reset (the boards are about to lose power, so the trim is NOT glided down first —
// owner decision 2026-11-25): pause (fans keep running) -> cool to <= 55 °C (no wait when already there) -> stop
// bosminer (it cuts the hashboard power) and set the trim to 0 directly at that moment (no load) -> act (e.g.
// write clocks into the saved profile) while it is stopped -> off time -> start -> re-apply the fan (a restart
// resets it; fan < minFan = the live fan rule's current fan) -> resume -> every enabled board hashing. Used by the
// latch recovery, the level's clean start, the search's resets and clock corrections. Target ~3-3.5 min.
func cleanApply(fan, gateTarget int, off time.Duration, act func() error) (err error) {
	wizEvent("clean", "soft reset at %d W: pause, cool, bosminer off %s with trim 0, on, resume", gateTarget, off)
	defer func() {
		if err != nil {
			wizEvent("clean", "soft reset: %v", err)
		} else {
			wizEvent("clean", "soft reset: every board hashing")
		}
	}()
	pre := read().rates // per-board rates before the reset: the "caught up" reference after it
	wizLive("soft reset: pause, cooling to ≤ 55 °C before cutting hashboard power")
	_, _ = bos("PUT", "/actions/pause", nil)
	waitFor(4*time.Minute, 10*time.Second, func() bool { r := read(); return r.ok && r.chip <= 55 })
	if stopRequested() { // stopped while cooling: nothing was cut yet
		_, _ = bos("PUT", "/actions/resume", nil)
		return wizStopped{}
	}
	wizLive("soft reset: bosminer off (board power cut), trim 0 %%")
	// the fans while bosminer is down / starting: fanGuard (fanhold.go), for every restart
	stop := exec.Command("/etc/init.d/bosminer", "stop")
	serr := stop.Start()
	trimOffNow() // at the moment the power is cut: no glide, there is no load; the PWM goes quiet at once
	if serr == nil {
		_ = stop.Wait() // prints an error on Braiins but stops it
	}
	waitUp(60*time.Second, 3*time.Second, func() bool { return exec.Command("pidof", "bosminer").Run() != nil })
	var actErr error
	if act != nil {
		actErr = act()
	}
	if !wizSleep(off) { // stopped while the boards are off: bosminer back on and resumed before returning
		_ = exec.Command("/etc/init.d/bosminer", "start").Run()
		waitUp(4*time.Minute, 10*time.Second, func() bool { return read().ok })
		_, _ = bos("PUT", "/actions/resume", nil)
		return wizStopped{}
	}
	_ = exec.Command("/etc/init.d/bosminer", "start").Run()
	waitUp(4*time.Minute, 10*time.Second, func() bool { return read().ok })
	if fan < minFan {
		fan = fanNow(0)
	}
	if fan >= minFan {
		_, _ = fanSet(fan)
	}
	_, _ = bos("PUT", "/actions/resume", nil)
	if actErr != nil {
		return actErr
	}
	gate := float64(gateTarget) / 1000 * 2.4
	if waitFor(6*time.Minute, 15*time.Second, func() bool { r := read(); return r.ok && r.mining && r.minRate > gate }) {
		// the fan again, now that bosminer is past its start-up minimum: the write before resume could not be
		// confirmed while bosminer held 25 %, and the fan stayed there for a whole level (2026-11-25 18:53)
		if fan >= minFan {
			if out, err := fanSetCheck(fan, fanSet, 2*time.Minute); err != nil {
				note("soft reset: fan %d %% not confirmed: %v %s", fan, err, out)
			}
		}
		// hashing again is not steady yet: the 1-min board rates still climb after a restart, and a step started now
		// reads that as a failure (two false fails + extra resets on the 2026-11-25 run). Wait for steady rates.
		if len(pre) > 0 {
			wizLive("soft reset: waiting for every board to reach %.0f %% of its rate before the reset", caughtUpFr*100)
			waitFor(6*time.Minute, 10*time.Second, func() bool { r := read(); return r.ok && caughtUp(r.rates, pre, caughtUpFr) })
		} else {
			wizLive("soft reset: waiting for steady board rates")
			waitSteadyRates(6 * time.Minute)
		}
		if stopRequested() {
			return wizStopped{}
		}
		return nil
	}
	if stopRequested() {
		return wizStopped{}
	}
	return fmt.Errorf("boards not hashing after the soft reset")
}

// errPowerCycle ends the whole run: boards that two power-off cycles could not recover need the plug pulled.
var errPowerCycle = fmt.Errorf("boards still not hashing after two power-off cycles: unplug the miner for 30 s, then restart the setup")

// wizChange: changeTo for the wizard (boards b, stock voltage), then the live fan rule's fan (fan < minFan) or fan. A failed hashing gate is
// never measured: the boards are recovered (clean power-off cycles) first; errPowerCycle if that fails.
func wizChange(t int, b string, fan int) error {
	_, err := changeTo(t, levelEntry{B: b}, nil)
	if stopRequested() { // never a recovery after a stop
		return wizStopped{}
	}
	if errors.Is(err, errNotHashing) {
		wizLive("%d W: a board is not hashing after the change - recovering before any measurement", t)
		err = recoverBoards(fan, t)
	}
	if err != nil {
		return err
	}
	setPhase("wizard")
	if fan < minFan {
		fan = fanNow(0)
	}
	if fan < minFan {
		return nil
	}
	if out, ferr := fanSet(fan); ferr != nil { // a board switch / restart resets bosminer's fan target
		note("wizard: fan re-apply: %v %s", ferr, out)
	}
	return nil
}

// actingDuty: the drive % where the voltage starts dropping on a sweep: the first point at least dv below the
// first point's voltage with the next point also below (one noisy read does not count). -1 = never.
func actingDuty(p []calPoint, dv float64) float64 {
	if len(p) == 0 {
		return -1
	}
	lim := p[0].V - dv
	for i := 1; i < len(p); i++ {
		if p[i].V <= lim && (i == len(p)-1 || p[i+1].V <= lim) {
			return p[i].Duty
		}
	}
	return -1
}

// actingDV: the drop that counts as "the mod acts" (well above the board voltage reading's noise).
const actingDV = 0.05

// sweepDetail: the sweep stage's line: where the voltage starts dropping, the floor (mean V at 100 %; or where and
// why the sweep ended), where boards stopped hashing at the test profile (information only), resistor warnings.
func sweepDetail(run calRun) string {
	if len(run.Points) == 0 {
		return "no points: " + run.Status
	}
	p0, fl := run.Points[0], run.Points[len(run.Points)-1]
	var d string
	if a := actingDuty(run.Points, actingDV); a >= 0 {
		d = fmt.Sprintf("the voltage starts dropping at %.0f %% (%.2f V at 0 %%)", a, p0.V)
	} else {
		d = "the voltage never dropped"
	}
	d += fmt.Sprintf("; floor %.2f V at %.0f %% trim", fl.V, fl.Duty)
	if run.Status != "done" {
		d += fmt.Sprintf(" (sweep ended at %.0f %%: %s)", fl.Duty, run.Status)
	}
	if p, ok := firstNotHashing(run.Points); ok {
		d += fmt.Sprintf("; boards stop hashing at %.0f %% (%.2f V) at the test profile (board %s; information only)",
			p.Duty, p.V, strings.Join(p.NotHashing, ", "))
	} else {
		d += "; every board hashed to the end"
	}
	switch {
	case fl.V < 8.3:
		d += " — LOWER than recommended (~8.8 V): the series resistor is too small; a stuck output could take the PSU too low"
	case fl.Duty >= 100 && fl.V > 9.6:
		d += " — HIGH: low levels may not reach their best voltage (series resistor too large?)"
	}
	return d
}

// firstNotHashing: the first sweep point where a board was not hashing.
func firstNotHashing(ps []calPoint) (calPoint, bool) {
	for _, p := range ps {
		if len(p.NotHashing) > 0 {
			return p, true
		}
	}
	return calPoint{}, false
}

// startDriveNone: the first step with nothing known (no curve, no point) starts at this drive.
const startDriveNone = 30

// seriesDrive: drive % for v along one set of measured points (sorted by V): linear inside the range (true);
// outside it, extended along the end segment (false). One point: its drive (false).
func seriesDrive(ps []drivePt, v float64) (float64, bool) {
	if len(ps) == 1 {
		return ps[0].D, false
	}
	for i := 1; i < len(ps); i++ {
		a, b := ps[i-1], ps[i]
		if v >= a.V && v <= b.V {
			if b.V == a.V {
				return a.D, true
			}
			return a.D + (v-a.V)/(b.V-a.V)*(b.D-a.D), true
		}
	}
	a, b := ps[0], ps[1] // below the range
	if v > ps[len(ps)-1].V {
		a, b = ps[len(ps)-2], ps[len(ps)-1]
	}
	if b.V == a.V {
		return a.D, false
	}
	return a.D + (v-a.V)/(b.V-a.V)*(b.D-a.D), false
}

// startDrive is the ONE place that answers "which drive % gives board voltage v at load w with boards b?" — the
// first drive of every level's search (a step is then a drive; its voltage is the hold's mean, never corrected), the
// local slope that converts the search's volt step sizes to drive (driveStep), the drive of a fallback, and (through
// the same model) the drive chart's fitted lines. In order:
//  1. a sweep curve exists: the fitted drive model (drivemodel.go: the sweep's smooth monotone shape, shifted in
//     drive by a constant + a load term fitted on every measured search step), inverted at v for load w;
//  2. no curve: measured search steps of the nearest load (same board set preferred): inside their voltage range,
//     interpolated; outside it, extended along their end segment;
//  3. the wiring and floor points (same rule as 2);
//  4. nothing known: startDriveNone.
func startDrive(curves []wizCurve, pts []drivePt, v float64, w int, b string) float64 {
	clamp := func(d float64) float64 { return math.Round(math.Max(0, math.Min(100, d))*100) / 100 }
	if m, ok := modelFor(curves, pts, w, b); ok {
		d, _ := m.Drive(v, float64(w))
		return clamp(d)
	}
	fromPoints := func(src func(drivePt) bool) (float64, bool) {
		var sel []drivePt
		for _, same := range []bool{true, false} {
			for _, p := range pts {
				if src(p) && p.V > 0 && (!same || p.B == b) {
					sel = append(sel, p)
				}
			}
			if len(sel) > 0 {
				break
			}
		}
		if len(sel) == 0 {
			return 0, false
		}
		near := sel[0].W // nearest load; ties go to the lower load
		for _, p := range sel {
			if d, dn := abs(p.W-w), abs(near-w); d < dn || (d == dn && p.W < near) {
				near = p.W
			}
		}
		g := []drivePt{}
		for _, p := range sel {
			if p.W == near && (len(g) == 0 || p.B == g[0].B) {
				g = append(g, p)
			}
		}
		sort.SliceStable(g, func(i, j int) bool { return g[i].V < g[j].V })
		d, _ := seriesDrive(g, v)
		return clamp(d), true
	}
	if d, ok := fromPoints(func(p drivePt) bool { return p.Src == "step" }); ok {
		return d
	}
	if d, ok := fromPoints(func(p drivePt) bool { return p.Src != "step" }); ok {
		return d
	}
	return startDriveNone
}

// addDrivePt records a measured point (rounded) in the wizard state.
// load: the point's load key for the drive model (T when set, else W).
func (p drivePt) load() int {
	if p.T > 0 {
		return p.T
	}
	return p.W
}

func addDrivePt(p drivePt) {
	if p.V <= 0 {
		return
	}
	p.V, p.D = math.Round(p.V*1000)/1000, math.Round(p.D*100)/100
	wizSet(func(s *wizState) { s.Points = append(s.Points, p) })
}

// ---- the run ----

// wizStageTitles: a run's stages in order (key, title).
var wizStageTitles = [][2]string{{"check", "Check the miner"}, {"fixed", "Fixed test profile"},
	{"sweep", "Trim sweep 0-100 % (mod check, drive chart, lowest voltage)"},
	{"droop", "Load droop (drive points at your lowest and highest wattage)"},
	{"tuner", "Back to Braiins' tuner"},
	{"search", "Efficiency search"}}

// wizRun runs the setup; resume = continue a saved stopped/interrupted run (resume.go: preparation skipped when
// it completed on the same miner, finished levels skipped); redoPrep = a resume whose preparation always runs again
// (results replaced, the tunes backup kept).
func wizRun(in wizInputs, resume, redoPrep bool) {
	coreMu.Lock()
	preW := effective
	coreMu.Unlock()
	preTarget := read().target
	// exit path (done, error, stop, panic): trim glides to 0, the miner resumes, then the core gets the miner back
	// at the pre-run level (or what the mode wants) with fans never lower than now (guard.go handBack)
	defer func() {
		if p := recover(); p != nil {
			note("wizard: PANIC %v", p)
			go alert("setup_panic", fmt.Sprintf("wizard crashed (%v): trim back to 0 %%, miner resumed", p))
			wizSet(func(s *wizState) {
				s.Message = fmt.Sprintf("internal error: %v", p)
				for i := range s.Stages {
					if s.Stages[i].Status == "running" {
						s.Stages[i].Status, s.Stages[i].Detail = "failed", "internal error"
					}
				}
			})
		}
		safeStop()
		leaveManual() // never leave the miner on the fixed manual profile: tuner mode at the pre-run target
		handBack(preW, preTarget)
		wizSet(func(s *wizState) { s.Running, s.Ended, s.Live, s.Remain = false, time.Now().Unix(), "", 0 })
	}()
	stage := func(key string) int {
		for i, st := range wiz.Stages {
			if st.Key == key {
				return i
			}
		}
		return -1
	}
	var title string // the stage's title for its events (set under wizMu)
	begin := func(key string) {
		wizSet(func(s *wizState) {
			i := stage(key)
			s.Stages[i].Status, s.Stages[i].Started = "running", time.Now().Unix()
			title = s.Stages[i].Title
		})
		wizEvent("stage", "%s: started", title)
	}
	end := func(key, status, detail string) {
		wizSet(func(s *wizState) {
			i := stage(key)
			s.Stages[i].Status, s.Stages[i].Detail, s.Stages[i].Ended = status, detail, time.Now().Unix()
			title = s.Stages[i].Title
		})
		wizEvent("stage", "%s: %s - %s", title, status, detail)
	}
	remain := func() {
		wizSet(func(s *wizState) {
			m := 0
			for _, st := range s.Stages {
				if st.Status == "waiting" {
					m += st.EstMin
				}
			}
			for _, l := range s.Levels {
				if l.Status == "waiting" {
					m += levelMin(false)
				}
			}
			s.Remain = m
		})
	}
	fail := func(key string, err error) {
		if _, ok := err.(wizStopped); ok {
			end(key, "failed", "stopped by user")
			wizSet(func(s *wizState) { s.Message = "stopped" })
			return
		}
		end(key, "failed", err.Error())
		wizSet(func(s *wizState) { s.Message = key + ": " + err.Error() })
		note("wizard: %s failed: %v", key, err)
	}

	// 1. check
	begin("check")
	r := read()
	if !r.ok && drv != nil {
		// bosminer may be restarting (e.g. a previous run left manual mode and the switch back restarts it):
		// wait up to checkWait for it to answer before giving up.
		wizLive("waiting for bosminer to answer (it may be restarting)")
		waitFor(checkWait, 10*time.Second, func() bool { r = read(); return r.ok || stopRequested() })
		if stopRequested() {
			fail("check", wizStopped{})
			return
		}
	}
	switch {
	case drv == nil:
		fail("check", fmt.Errorf("no trim output: this control board is not the tested layout (see the guide)"))
		return
	case !r.ok:
		fail("check", fmt.Errorf("bosminer does not answer"))
		return
	}
	if _, err := bos("GET", "/miner/hw/hashboards", nil); err != nil {
		fail("check", fmt.Errorf("Braiins API login failed: %v (check bos_user/bos_pass in web.json)", err))
		return
	}
	mt, mtSrc := minPowerTarget()
	end("check", "done", fmt.Sprintf("trim output ok, bosminer at %d W, boards %s; minimum power target %d W (%s)", r.target, r.enabled, mt, mtSrc))
	id := minerIdentity(mt)
	skipPrep, droopOnly := false, false
	if resume {
		plan, why := resumePlan(wizSnap(), id, redoPrep)
		skipPrep, droopOnly = plan != "full", plan == "droop"
		switch plan {
		case "droop":
			wizEvent("stage", "resume: preparation done on this miner, no droop points yet - the droop stage runs (no sweep)")
			wizSet(droopOnlyStages)
		case "none":
			wizEvent("stage", "resume: preparation done on this miner - skipped")
		default:
			wizEvent("stage", "resume: preparation runs again (%s)", why)
			wizSet(resetPrep)
		}
	}
	wizSet(func(s *wizState) { s.Miner = &id })
	// bosminer may still be paused: a plug pulled during a soft reset (which pauses first) comes back paused, because
	// Braiins keeps a pause across a reboot. A paused bosminer does not follow the fan setting, so the fan check below
	// could never pass (2026-11-25 power-loss test). Resume is harmless on a miner that is already mining.
	if r := read(); !r.mining {
		wizLive("bosminer is paused: resuming mining before the fan check")
		_, _ = bos("PUT", "/actions/resume", nil)
		waitFor(3*time.Minute, 10*time.Second, func() bool { return read().mining })
	}
	if out, err := fanSetCheck(in.MaxFan, fanSet, 3*time.Minute); err != nil {
		fail("check", fmt.Errorf("fan set: %v %s", err, out))
		return
	}
	fanRuleStart(in.MaxFan) // the live fan rule takes over from the maximum fan (core.go fanRule)
	remain()

	restoreW := preTarget // the target tuner mode comes back at
	if restoreW <= 0 {
		restoreW = midW(in)
	}
	// 3b. load droop (droop.go): manual mode at the user's lowest and highest wattage, 3 drive points each; the trim
	// ends at 0 and manual mode stays on (the tuner stage follows). Only a stop or a power cycle ends the run; any
	// other problem is noted on the stage and the run goes on (the model then fits k from the levels' steps).
	droop := func() bool {
		begin("droop")
		s := wizSnap()
		c := sweepCurveOf(s)
		switch {
		case s.Fixed != nil && s.Fixed.Fallback != "":
			end("droop", "skipped", "manual mode was refused on this miner")
			return true
		case c == nil:
			end("droop", "skipped", "no sweep")
			return true
		}
		fixT, _ := fixedProfile()
		volt := fixedVoltFallback
		if s.Fixed != nil && s.Fixed.Volt > 0 {
			fixT, volt = s.Fixed.Target, s.Fixed.Volt
		} else {
			volt, _ = fixedVoltage()
		}
		detail, err := runDroop(in, *c, fixT, volt, restoreW)
		_ = setTrim(0)
		if err != nil {
			if _, stop := err.(wizStopped); stop || err == errPowerCycle {
				fail("droop", err)
				return false
			}
			end("droop", "failed", strings.TrimPrefix(detail+"; "+err.Error()+" (the run goes on)", "; "))
			remain()
			return true
		}
		end("droop", "done", detail)
		remain()
		return true
	}
	var tunerStage func() bool // 4. below
	prepare := func() bool {
		// 2. fixed test profile: Braiins' tuner off, one conservative clock + generous voltage for every chip
		// (fixedmode.go), so the mod is proven within minutes. If manual mode fails: the old tuned path (Braiins tunes the bottom of the range).
		begin("fixed")
		fixT, fixMHz := fixedProfile()
		fixV, fixVSrc := fixedVoltage() // Braiins' minimum PSU voltage: the setpoint every tuned level trims from
		fx := &wizFixed{MHz: fixMHz, Volt: fixV, VoltSrc: fixVSrc, Target: fixT}
		low, _ := clampTarget(in.MinW, mt)            // the fallback's level
		testW := int(math.Round(estWall(fixT, fixV))) // the load the early stages run at (est. wall W)
		wizLive("manual mode: %.0f MHz on all chips, %.2f V = Braiins' minimum (%s; tuner off)", fixMHz, fixV, fixVSrc)
		err := setManualMode(fixMHz, fixV, restoreW)
		if err == nil {
			// The manual-mode switch restarts bosminer: the PSU starts high (~14.6 V) and steps down to the requested
			// voltage while the chips ramp their clocks up from ~70 MHz. "Every board hashing" is true halfway through
			// that ramp, so wait until the clocks have reached the profile and bosminer has stopped moving the PSU:
			// voltage readings are wrong while the PSU steps (first test run 2026-11-25 failed on this). No steady-rate
			// wait here (owner 2026-11-25): the sweep needs no reference rate (its hashing flags are information only).
			wizLive("manual mode on: waiting until every board hashes")
			if err = manualSettle(fixMHz, fixT); err == errPowerCycle {
				fail("fixed", err)
				return false
			} else if _, stopped := err.(wizStopped); stopped {
				fail("fixed", err)
				return false
			}
		}
		if err != nil {
			if _, stopped := err.(wizStopped); stopped {
				fail("fixed", err)
				return false
			}
			note("wizard: fixed profile failed (%v) - back to tuner mode, Braiins tunes %d W instead", err, low)
			if rerr := restoreTunerMode(restoreW); rerr != nil {
				note("wizard: %v", rerr)
			}
			fx = &wizFixed{Fallback: err.Error()}
			testW = low
			wizLive("fallback: changing to %d W (3 boards, stock voltage); Braiins tunes it from scratch", low)
			if err := wizChange(low, "123", 0); err != nil {
				fail("fixed", err)
				return false
			}
		}
		wizSet(func(s *wizState) { s.Fixed = fx })
		if fx.Fallback == "" {
			end("fixed", "done", fmt.Sprintf("%.0f MHz on every chip at %.2f V requested (Braiins' minimum, %s; ~%d W target), every board hashing", fixMHz, fixV, fixVSrc, fixT))
		} else {
			end("fixed", "done", fmt.Sprintf("manual mode failed (%s): ran Braiins' tune at %d W instead", fx.Fallback, low))
		}
		remain()

		// 3. trim sweep (same profile): 0 -> 100 % in sweepStep steps, the board voltage read after a short settle at
		// every step. It is also the trim test (owner 2026-11-25, the separate 50 % stage is gone): as soon as a point is
		// modDV below the 0 % reading the page shows "the mod works"; no such point by modBy % stops the sweep and fails
		// the stage. Boards that stop hashing do NOT end it (the floor belongs to the PSU + series resistor; a board
		// dropping out at the test clock says nothing about low-clock levels): flagged per point, information only.
		// The points are the drive chart and startDrive's curve; the floor = the mean voltage at 100 %. Afterwards the
		// trim glides back to 0; no hashing gate here: the next stage puts Braiins' tuner back, which restarts bosminer
		// and clears any chip the deep end latched (and the search starts with a soft reset anyway).
		begin("sweep")
		label := fmt.Sprintf("test profile %.0f MHz, ~%d W target", fixMHz, fixT)
		if fx.Fallback != "" {
			label = fmt.Sprintf("Braiins' tune at %d W (fallback)", testW)
		}
		wizLive("trim sweep 0-100 %% in %.0f %% steps, %.0f s each (%.0f s settle + %.0f s averaged) (%s)", sweepStep,
			sweepSettle+1.5*float64(sweepReads-1), sweepSettle, 1.5*float64(sweepReads-1), label)
		wizEvent("sweep", "trim sweep 0-100 %% in %.0f %% steps on the %s", sweepStep, label)
		onMod := func(stock, v, d float64) {
			msg := modText(stock, v, d)
			wizSet(func(s *wizState) {
				s.Mod = &wizMod{StockV: math.Round(stock*1000) / 1000, V: math.Round(v*1000) / 1000, Duty: d}
				if i := stage("sweep"); i >= 0 {
					s.Stages[i].Detail = msg + " (sweep continues)"
				}
			})
			wizEvent("sweep", "%s", msg)
		}
		run := sweepCurve(calRun{Start: 0, Stop: 100, Step: sweepStep, Settle: sweepSettle, Reads: sweepReads, DropTrim: true, HashLimit: dropFr,
			KeepGoing: true, ModDV: modDV, ModBy: modBy, OnMod: onMod, Label: label})
		if calStopped() || run.Status == "stopped" { // a stopped partial sweep is not kept
			fail("sweep", wizStopped{})
			return false
		}
		if len(run.Points) == 0 {
			fail("sweep", fmt.Errorf("no voltage reading (%s)", run.Status))
			return false
		}
		curve := wizCurve{Watts: testW, StockV: run.StockV, Points: run.Points, Status: run.Status}
		wizSet(func(s *wizState) { s.Curves = append(s.Curves, curve) }) // kept even on a failed mod check (diagnosis)
		if run.ModFailed {
			fl := run.Points[len(run.Points)-1]
			fail("sweep", fmt.Errorf("the mod is not acting on the voltage — check the wiring (%.2f → %.2f V at %.0f %%)", run.Points[0].V, fl.V, fl.Duty))
			return false
		}
		fl := run.Points[len(run.Points)-1]
		if err := sweepComplete(fl.Duty, wizSnap().Mod != nil, run.Status); err != nil { // an early end is never a floor
			fail("sweep", err)
			return false
		}
		floor := fl.V
		wizSet(func(s *wizState) { s.Floor = math.Round(floor*100) / 100 })
		addDrivePt(drivePt{W: testW, V: floor, D: fl.Duty, B: "123", Src: "floor", L: -1})
		wizEvent("sweep", "%d points, %s", len(run.Points), run.Status)
		end("sweep", "done", sweepDetail(run))
		remain()
		if !droop() {
			return false
		}
		return tunerStage()
	}
	tunerStage = func() bool {
		// 4. back to Braiins' tuner at the pre-run target, then the saved tunes moved aside (bosminer restarts) so every
		// level is tuned fresh. The sweep went deep: the search's first level starts with a clean soft power
		// cycle (searchLevel step 1, cleanApply) before any measurement.
		begin("tuner")
		tdetail := "tuner mode was on (fallback)"
		if manualRestoreW() > 0 {
			glideZero()
			wizLive("back to Braiins' tuner mode at %d W", restoreW)
			if err := restoreTunerMode(restoreW); err != nil {
				fail("tuner", err)
				return false
			}
			tdetail = fmt.Sprintf("tuner mode at %d W", restoreW)
		}
		if bk, move := tunesStep(wizSnap()); bk != "" {
			tdetail += "; saved Braiins tunes: the backup from the first start is kept (" + bk + ")"
		} else if move {
			wizLive("moving saved Braiins tunes to a backup (bosminer restarts)")
			bk, err := moveBraiinsTunes()
			if err != nil {
				fail("tuner", err)
				return false
			}
			wizSet(func(s *wizState) { s.TunesBackup = bk })
			tdetail += "; saved Braiins tunes -> " + bk + " (restore: copy it back to " + autotuneFile + " with bosminer stopped)"
		}
		end("tuner", "done", tdetail)
		remain()
		return true
	}
	switch {
	case !skipPrep:
		if !prepare() {
			return
		}
	case droopOnly: // resume: preparation reusable but measured before the droop stage existed
		if !droop() || !tunerStage() {
			return
		}
	}

	// 5. search, low to high power
	begin("search")
	curves := wizSnap().Curves
	if len(wizSnap().Levels) == 0 { // a resumed run keeps its levels
		for _, j := range in.jobs() {
			jb := j
			wizSet(func(s *wizState) {
				s.Levels = append(s.Levels, wizLevel{Watts: jb.W, Boards: jb.B, Status: "waiting", Index: len(s.Levels)})
			})
		}
	}
	remain()
	tooHot := 0 // lowest level found too hot: higher levels are skipped
	prev := map[string]*prevRes{}
	for idx := range wizSnap().Levels {
		lv := wizSnap().Levels[idx]
		upd := func(f func(l *wizLevel)) { wizSet(func(s *wizState) { f(&s.Levels[idx]) }) }
		if stopRequested() {
			fail("search", wizStopped{})
			return
		}
		if lv.Status != "waiting" { // finished before a resume: only its results carry over
			if lv.Status == "done" && lv.V > 0 {
				prev[lv.Boards] = &prevRes{W: float64(lv.Watts), V: lv.V}
			}
			if lv.Status == "too_hot" && (tooHot == 0 || lv.Watts < tooHot) {
				tooHot = lv.Watts
			}
			if idx == 0 && len(wizSnap().Ranking) == 0 {
				rk := rankBoards(lv)
				wizSet(func(s *wizState) { s.Ranking = rk })
			}
			continue
		}
		if tooHot > 0 && lv.Watts >= tooHot {
			upd(func(l *wizLevel) {
				l.Status, l.Note = "skipped", "above the highest level that stays under the temperature target"
			})
			continue
		}
		if strings.HasPrefix(lv.Boards, "n") { // resolve n strongest boards from the ranking
			n, _ := strconv.Atoi(lv.Boards[1:])
			rk := wizSnap().Ranking
			if len(rk) < n {
				upd(func(l *wizLevel) { l.Status, l.Note = "skipped", "no board ranking" })
				continue
			}
			bs := append([]string(nil), rk[:n]...)
			sort.Strings(bs)
			lv.Boards = strings.Join(bs, "")
			upd(func(l *wizLevel) { l.Boards = lv.Boards })
		}
		t0 := time.Now()
		upd(func(l *wizLevel) { l.Status = "running" })
		lv.Status = "running" // searchLevel works on this copy and writes it back: it must say running too (the page
		// showed a running level as "waiting" with its timeline collapsed)
		wizEvent("level", "%d W, boards %s: started", lv.Watts, lv.Boards)
		res, err := searchLevel(lv, in, curves, upd, prev[lv.Boards])
		endLevel(upd, &res, err)
		wizEvent("level", "%d W, boards %s: %s", lv.Watts, lv.Boards, levelResult(res, err))
		if err == nil && stopRequested() { // a stop that a wait swallowed: the level is not finished
			err = wizStopped{}
		}
		if err != nil {
			if _, ok := err.(wizStopped); ok || err == errPowerCycle { // interrupted: a resume restarts this level
				upd(func(l *wizLevel) { l.Status, l.Note = "interrupted", err.Error() })
				fail("search", err)
				return
			}
			upd(func(l *wizLevel) { l.Status, l.Note = "failed", err.Error() })
			continue
		}
		upd(func(l *wizLevel) { *l = res; l.Minutes = time.Since(t0).Minutes() })
		wizSet(func(s *wizState) { pickChosen(s.Levels) }) // its row in the tuned-levels table, at once
		if n, err := applyWizTable(); err != nil {         // and in the heater's level table: no Apply button
			note("wizard: %d W: level table not written: %v", lv.Watts, err)
		} else {
			wizEvent("level", "%d W: written to the heater's level table (%d rows from this run)", lv.Watts, n)
		}
		if res.Status == "done" && res.V > 0 {
			prev[res.Boards] = &prevRes{W: float64(res.Watts), V: res.V}
		}
		if res.Status == "too_hot" && (tooHot == 0 || res.Watts < tooHot) {
			tooHot = res.Watts
		}
		if idx == 0 { // the first job (3 boards, bottom of the range) ranks the boards
			rk := rankBoards(res)
			wizSet(func(s *wizState) { s.Ranking = rk })
			note("wizard: board ranking (strongest first at low voltage): %v", rk)
		}
		remain()
	}
	// final pass: board-count pick + voltage rising with power; notes on the affected rows (resume.go)
	wizSet(func(s *wizState) { s.FinalCheck = finalCheck(s.Levels) })
	end("search", "done", fmt.Sprintf("%d levels searched", len(wizSnap().Levels)))
	wizSet(func(s *wizState) { s.Message = "done: the tuned levels are ready to apply" })
}

// caughtUpFr: after a soft reset the next step starts once every board is back to this share of its rate before the
// reset. The 1-min board rates lag a restart; a step started earlier read a ramping board as below 60 % of stock and
// aborted at 0 % trim (run 3, 2026-11-25). Replaces the 3-readings-within-3 % wait (~5 min) with the ramp itself.
const caughtUpFr = 0.90

// caughtUp: every board that had a rate before the reset is back to fr of it.
func caughtUp(now, pre map[string]float64, fr float64) bool {
	if len(now) == 0 {
		return false
	}
	for b, p := range pre {
		if p > 0 && now[b] < fr*p {
			return false
		}
	}
	return true
}

// voltAtDrive: the voltage a drive is expected to give (inverse of the level's drive(v) map), for the live line.
func voltAtDrive(drive func(float64) float64, d float64) float64 {
	lo, hi := 7.0, 13.0 // drive rises as the voltage falls
	for i := 0; i < 40; i++ {
		mid := (lo + hi) / 2
		if drive(mid) > d {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// applyWizTable writes the run's finished rows into the heater's level table (core.json); rows the run has not
// produced are kept. Called after every finished level, so the table fills as the run goes. No separate backup: the
// old rows only fit the old Braiins tunes, which the run moved aside (that backup is the way back).
func applyWizTable() (int, error) {
	s := wizSnap()
	t := wizTable(s)
	if len(t) == 0 {
		return 0, fmt.Errorf("no tuned level yet")
	}
	coreMu.Lock()
	ccfg.Levels = mergeTable(ccfg.Levels, t)
	ccfg.FanMax, ccfg.FanMin = s.Inputs.MaxFan, minFan
	coreMu.Unlock()
	if err := saveCoreConfig(""); err != nil {
		return 0, fmt.Errorf("save: %v", err)
	}
	wcfg.Levels = tableLevels()
	wizSet(func(s *wizState) { s.Applied = time.Now().Unix() })
	return len(t), nil
}

// fanSetCheck: s19regs confirms the live fan address by seeing the PWM move, so a target equal to the current PWM
// cannot be confirmed (run 7, 2026-11-25: bosminer had just started and held its 25 % minimum; setting the 25 %
// maximum failed on all 16 candidates). Nudge 2 % away first, then the target; retry until max (bosminer may still
// be starting).
func fanSetCheck(pct int, set func(int) (string, error), max time.Duration) (string, error) {
	var out string
	var err error
	deadline := time.Now().Add(max)
	for {
		nudge := pct - 2
		if nudge < minFan {
			nudge = pct + 2
		}
		if out, err = set(nudge); err == nil {
			if out, err = set(pct); err == nil {
				return out, nil
			}
		}
		if time.Now().After(deadline) || stopRequested() {
			return out, err
		}
		wizLive("fan check: retrying (%v)", err)
		if !wizSleep(20 * time.Second) {
			return out, err
		}
	}
}

// sweepComplete: the sweep counts only if it reached 100 % and the mod check passed. Run 7 (2026-11-25): bosminer
// restarted mid-sweep, the sweep ended at 4 % on "no voltage reading", and that was kept as the floor (11.82 V).
func sweepComplete(lastDuty float64, modOK bool, status string) error {
	if lastDuty < 100-sweepStep/2 {
		return fmt.Errorf("the sweep ended early at %.0f %% (%s)", lastDuty, status)
	}
	if !modOK {
		return fmt.Errorf("the voltage never dropped by the end of the sweep — check the wiring")
	}
	return nil
}

// modText: the sweep's mod-check result, e.g. "✓ the mod works: 11.96 → 11.79 V at 34 %".
func modText(stock, v, d float64) string {
	return fmt.Sprintf("✓ the mod works: %.2f → %.2f V at %.0f %%", stock, v, d)
}

// holdText: one measured step for the event list.
func holdText(st wizStep) string {
	if st.Dropped != "" {
		return fmt.Sprintf("drive %.1f %%: aborted - %s", st.Drive, st.Dropped)
	}
	kind := "short check"
	if st.Full {
		kind = fmt.Sprintf("%d-min hold", holdMin)
	}
	res := "pass"
	if !st.Pass {
		res = "FAIL"
	}
	mcr := ""
	if st.MCR > 0 {
		mcr = fmt.Sprintf(", step mcr %.1f %%", st.MCR*100)
	}
	fan := ""
	if st.Fan > 0 {
		fan = fmt.Sprintf(", fan %d %%", st.Fan)
	}
	return fmt.Sprintf("drive %.1f %% -> %s, %s: %.2f TH/s, est. %.2f J/TH, slowest board %.0f %%, chip %.0f °C%s, %d new weak chips%s, %s",
		st.Drive, vAvgText(st.V), kind, st.THs, st.JTH, st.Share*100, st.Chip, fan, st.NewWeak, mcr, res)
}

// latchText: the latch check's result for the event list.
func latchText(known bool, latched []string, down bool) string {
	s := "no per-chip data"
	if known {
		s = fmt.Sprintf("%d chips latched", len(latched))
		if len(latched) > 0 {
			s += " (" + strings.Join(latched, ", ") + ")"
		}
	}
	if down {
		s += ", a board down"
	}
	return s
}

// levelResult: a finished level for the event list.
func levelResult(l wizLevel, err error) string {
	if err != nil {
		return "failed: " + err.Error()
	}
	s := l.Status
	if l.V > 0 {
		s += fmt.Sprintf(" at %.2f V (%s), est. %.0f W, %.1f J/TH", l.V, l.Answer, l.EstW, l.EstJTH)
	} else if l.Status == "done" || l.Status == "unsafe" {
		s += " at stock voltage"
	}
	if l.Note != "" {
		s += "; " + l.Note
	}
	return s
}

// prevRes: the last resolved level for a board set (prediction for the next one: +100 W needs only a little more V).
type prevRes struct{ W, V float64 }

// load: the drive-point key of this level (bosminer target x the clock factor in force).
func (r *levelRun) load() int { return int(math.Round(float64(r.t0) * r.scale)) }

// drive: the starting drive for voltage v at this level (startDrive over the curves and every measured point).
func (r *levelRun) drive(v float64) float64 {
	return startDrive(r.curves, wizSnap().Points, v, r.load(), r.l.Boards)
}

// levelRun is one level's search state: the fresh-tune baseline every step is judged against and the guards.
type levelRun struct {
	in         wizInputs
	l          *wizLevel
	upd        func(func(*wizLevel))
	curves     []wizCurve
	t0         int
	base       map[string]float64 // per-board stock TH/s of the fresh tune
	baseTH     float64
	baseWeak   map[string]bool      // chips weak in the fresh tune (nil = no per-chip data)
	baseChip   map[string][]float64 // per-chip work in the fresh tune (step mcr); baseSrc = its source
	baseSrc    string
	scale      float64   // clock factor in force (TH/s and power scale with it)
	bestJTH    float64   // best J/TH of a passing step so far (starts at stock): short checks above it extend
	since      time.Time // profile guard: bosminer must not touch the voltage/tune after this
	snap       string    // profile guard: Braiins' saved tune for t0 after the fresh tune
	sinceReset []int     // latch check: indices of the steps measured since the last soft reset (latchStart)
}

// liveRaw: a live line as is (the step line: "800 W · step 3 · testing ≈ 10.05 V"; the page adds the time left).
func (r *levelRun) liveRaw(line string) { wizLive("%s", line) }

func (r *levelRun) live(format string, a ...interface{}) {
	wizLive("%d W, boards %s: %s", r.l.Watts, r.l.Boards, fmt.Sprintf(format, a...))
}

// settled: wait for a settled stage-4 round (no "Setting voltage" since the round before) after since, at most
// settleTimeout (then proceed and note it), then restart the guard window. A stop ends it with wizStopped.
func (r *levelRun) settled(since time.Time) error {
	r.live("waiting for a settled Braiins stage-4 round (no voltage change for a whole round, up to %.0f min)", settleTimeout.Minutes())
	r.phase("settle", "Settled round wait", 0)
	defer r.phaseEndKind("settle")
	if !waitSettled(r.t0, since, settleTimeout) {
		if stopRequested() {
			return wizStopped{}
		}
		r.noSettle()
	}
	r.since = time.Now()
	return nil
}

// noSettle: no settled stage-4 round within settleTimeout: proceed, noted in the log and the level.
func (r *levelRun) noSettle() {
	note("wizard: %d W: no settled stage-4 round within %.0f min (proceeding)", r.l.Watts, settleTimeout.Minutes())
	r.l.Note = strings.TrimPrefix(r.l.Note+fmt.Sprintf("; proceeded without a settled stage-4 round (%.0f min)", settleTimeout.Minutes()), "; ")
}

// baseline: the settled-round wait after the level change (tChange) with the stock baseline measured DURING it as a
// sliding holdMin window (owner decision 2026-11-25). From Braiins' stage 4 at t0, every 15 s: board readings and
// the metrics text into a rolling buffer; a "Setting voltage" or a retune line discards it (the window restarts).
// The settled-round rule is unchanged (waitSettled's condition and timeout). Once it holds, the baseline = the last
// holdMin of the buffer if it covers that, else measuring continues (still discarding) until it does. The values
// mean exactly what the separate 5-min window gave (windowRates / windowChips, as avgRates). Sets r.snap.
func (r *levelRun) baseline(tChange time.Time) (map[string]float64, float64, float64, float64, error) {
	hold := time.Duration(holdMin) * time.Minute
	done := waitingOnBraiins(r.t0, "a settled stage-4 round (no voltage change for a whole round)")
	defer func() {
		if done != nil {
			done()
		}
	}()
	var w baseWindow
	t0 := time.Now()
	collecting, settled := false, false
	for {
		now := time.Now()
		stMu.Lock()
		s4, ok := tune.stage4Since(r.t0, tChange), tune.settledSince(r.t0, tChange)
		stMu.Unlock()
		if !settled && (ok || now.Sub(t0) >= settleTimeout) {
			settled = true
			if !ok {
				r.noSettle()
			}
			done()
			done = nil
			r.snap, _ = profileSnapshot(r.t0, len(r.l.Boards))
		}
		if !collecting && (s4 || settled) {
			collecting = true
			w.restart(now)
			r.phase("baseline", "Settled round + stock baseline", hold)
		}
		if collecting {
			if set, retune := bosEventsSince(w.start); set != "" || retune != "" {
				what := "moved the voltage"
				if retune != "" {
					what = "retuned"
				}
				wizEvent("baseline", "%d W: baseline window restarted: Braiins %s", r.l.Watts, what)
				w.restart(now)
			}
			rd := read()
			w.add(rateSample{at: time.Now(), r: rd, m: scrapeChips(), dv: readDevs()}, hold)
		}
		if settled {
			if mw, ok := w.last(hold); ok {
				rates, tot, chip, v, err := summarize(mw)
				if counterReset(err) { // bosminer restarted under the window: discard it, measure again
					wizEvent("baseline", "%d W: baseline window restarted: %v", r.l.Watts, err)
					w.restart(now)
				} else if err != nil {
					return nil, 0, 0, 0, err
				} else {
					r.phaseDone("done", fmt.Sprintf("stock %.2f V avg, %.1f TH/s", v, tot))
					return rates, tot, chip, v, nil
				}
			}
		}
		head := "waiting for a settled stage-4 round"
		if settled {
			head = "settled stage-4 round reached"
		}
		if collecting {
			r.live("%s; baseline %.1f / %d min collected", head, w.collected(hold).Minutes(), holdMin)
		} else {
			r.live("%s; baseline starts at Braiins stage 4 (up to %.0f min)", head, settleTimeout.Minutes())
		}
		if !wizSleep(15 * time.Second) {
			return nil, 0, 0, 0, wizStopped{}
		}
	}
}

// clean: a soft reset (cleanApply) at this level's target (act runs while bosminer is stopped); boards not hashing
// afterwards -> the latch recovery. Then wait for stage 4. Returns act's error, a stop, or errPowerCycle.
func (r *levelRun) clean(act func() error) error {
	r.resetPhase(act != nil)
	defer r.phaseEndKind("reset")
	t := time.Now()
	var actErr error
	err := cleanApply(0, r.t0, cleanOff, func() error {
		if act != nil {
			actErr = act()
		}
		return actErr
	})
	if _, stopped := err.(wizStopped); stopped {
		return err
	}
	r.sinceReset = nil               // the boards lost power: no step's latches carry over
	if err != nil && actErr == nil { // boards not hashing after the soft reset
		r.live("boards not hashing after the soft reset: recovering")
		if rerr := recoverBoards(0, r.t0); rerr != nil {
			return rerr
		}
	}
	// No settled-round wait after a soft reset at the same target: Braiins reloads the profile this level already
	// proved settled (a whole round without a voltage change, then the baseline and the first step). Wait only for
	// stage 4 and for the restart's PSU stepping to end; a voltage move during the next step is still caught by
	// measure() and that step redone.
	if act != nil { // the profile was changed while bosminer was stopped (clock correction): not proven yet
		if err := r.settled(t); err != nil {
			return err
		}
		return actErr
	}
	if err := r.afterReset(t); err != nil {
		return err
	}
	return actErr
}

// afterReset: stage 4 again at the level's target after t and no "Setting voltage" for resetQuiet; then the
// step window (r.since) starts now, so the restart's own PSU stepping is not counted against the next step.
func (r *levelRun) afterReset(t time.Time) error {
	r.live("soft reset done: waiting for Braiins' stage 4 (saved profile) and the PSU to stop stepping")
	ok := waitFor(settleTimeout, 10*time.Second, func() bool {
		if stopRequested() {
			return true
		}
		stMu.Lock()
		defer stMu.Unlock()
		return tune.stage4Since(r.t0, t) && time.Since(tune.lastSet) >= resetQuiet
	})
	if stopRequested() {
		return wizStopped{}
	}
	if !ok {
		r.noSettle()
	}
	r.since = time.Now()
	return nil
}

// checkWait: how long the check stage waits for bosminer to answer before failing.
const checkWait = 3 * time.Minute

// resetQuiet: how long bosminer must not have moved the PSU voltage after a restart before the next step.
const resetQuiet = 30 * time.Second

// fatal: errors that end the run (stop, power cycle needed).
func fatal(err error) bool {
	_, stopped := err.(wizStopped)
	return stopped || err == errPowerCycle
}

// judge one measured window against the fresh tune (the step rule: stepPass).
func (r *levelRun) judge(d float64, rates map[string]float64, th, chip, vm float64, full bool) wizStep {
	st := wizStep{V: math.Round(vm*100) / 100, Drive: math.Round(d*100) / 100, THs: th, Chip: chip, Share: 1,
		Shares: map[string]float64{}, Full: full, Fan: fanNow(0)}
	for b, r0 := range r.base {
		if r0 > 0 {
			st.Shares[b] = math.Round(rates[b]/(r0*r.scale)*1000) / 1000
			st.Share = math.Min(st.Share, rates[b]/(r0*r.scale))
		}
	}
	if th > 0 {
		st.JTH = math.Round(estWallF(r.t0, vm, r.scale)/th*100) / 100
	}
	if r.baseWeak != nil && lastChips != nil {
		st.Weak = newWeakList(weakChips(lastChips, lastFreqs, weakFr), r.baseWeak)
		st.NewWeak = len(st.Weak)
	}
	if m, per, ok := stepMCR(lastChips, r.baseChip, lastChipSrc, r.baseSrc); ok { // recorded for information
		st.MCR, st.MCRs = m, per
		if lastChipSrc == "shares" {
			st.MCRTH = sharesTHs(lastChips)
		}
	}
	st.Pass = stepPass(st.NewWeak, th, r.baseTH*r.scale, chip, r.in.ChipTarget)
	st.Stable = st.Pass
	return st
}

// hold measures one drive (unchanged rules): glide to d at the shared pace, no corrections, then the hold. The
// step's voltage = the hold's mean board voltage (overall and per board). Short checks can only say "continue": one
// that looks off (fails the step rule or does not improve J/TH) keeps measuring at the same drive until it has
// holdMin minutes (extended, not restarted). A board below 60 % of its stock rate aborts the hold: trim glides to
// 0, step failed.
func (r *levelRun) hold(d float64, short bool) (wizStep, error) {
	d = math.Max(0, math.Min(ccfg.TrimCap, d))
	r.stepPhase(d, short)
	dropped := func(e errBoardDrop) (wizStep, error) {
		r.live("drive %.1f %%: %v - hold aborted, trim back to 0 %%", d, e)
		glideZero()
		return wizStep{Drive: r2(d), Dropped: e.Error(), Full: !short}, nil
	}
	if err := glide(d, wizSleep, func() bool { return dropCheck(read(), r.base) != nil }); err != nil {
		if errors.Is(err, errAborted) {
			if e, ok := dropCheck(read(), r.base).(errBoardDrop); ok {
				return dropped(e)
			}
		}
		if e, ok := err.(errBoardDrop); ok {
			return dropped(e)
		}
		return wizStep{}, err
	}
	first := time.Duration(holdMin) * time.Minute
	if short {
		first = time.Duration(shortMin) * time.Minute
	}
	// a bosminer restart under the hold (its Total MH counters went backwards): the window is invalid, the step is
	// thrown out and redone (measure, like a "Setting voltage")
	invalid := func(err error) (wizStep, error) {
		return wizStep{Drive: r2(d), Invalid: err.Error(), Full: !short}, nil
	}
	r.phaseMark("hold", first)
	rates, th, chip, vm, err := avgRates(first, r.base)
	if e, ok := err.(errBoardDrop); ok {
		return dropped(e)
	} else if counterReset(err) {
		return invalid(err)
	} else if err != nil {
		return wizStep{}, err
	}
	bv := mixVolts(lastBVolts, nil, 1, 0)
	st := r.judge(d, rates, th, chip, vm, !short)
	if short && !(st.Pass && st.JTH < r.bestJTH) {
		r.live("drive %.1f %% (%.2f V avg) looked off in the short check - extending to %d min", d, vm, holdMin)
		r.phaseMark("hold", time.Duration(holdMin)*time.Minute)
		d0, chips1, src1 := lastDevs[0], lastChips, lastChipSrc
		r2s, th2, chip2, vm2, err := avgRates(time.Duration(holdMin-shortMin)*time.Minute, r.base)
		if e, ok := err.(errBoardDrop); ok {
			return dropped(e)
		} else if counterReset(err) {
			return invalid(err)
		} else if err != nil {
			return wizStep{}, err
		}
		wa, wb := float64(shortMin)/float64(holdMin), float64(holdMin-shortMin)/float64(holdMin)
		for b := range rates {
			rates[b] = rates[b]*wa + r2s[b]*wb
		}
		th = th*wa + th2*wb
		// the whole hold as one window: Total MH from the first part's start to the extension's end
		if xr, xt, _, err := exactRates(d0, lastDevs[1], rates, th); counterReset(err) {
			return invalid(err)
		} else {
			rates, th = xr, xt
		}
		if src1 == lastChipSrc {
			lastChips = mixChips(chips1, lastChips, wa, wb)
		}
		bv = mixVolts(bv, lastBVolts, wa, wb)
		st = r.judge(d, rates, th, math.Max(chip, chip2), vm*wa+vm2*wb, true)
	}
	st.BV = bv
	addChipSnap(r.l.Index, fmt.Sprintf("%.1f %% (%.2f V)", st.Drive, st.V), st.V, st.Pass)
	return st, nil
}

// measure: hold (one drive) + profile guard (gap-review rulings + the 26.11 log, 2026-11-25). bosminer events since the last good
// step are checked before the hold and after it:
//   - a "Setting voltage" (stage-4 settling) moved the board voltage under the hold: the step is thrown out, trim
//     glides to 0, wait for a settled round, redo; no own limit;
//   - a real retune (stage 1-3, profile failed, restart): same, but only one per level;
//   - either way at most maxRedos thrown-out steps per level, then the level fails (touchVerdict).
//
// A "Setting voltage" before the hold (between steps, nothing measured) only waits for a settled round.
func (r *levelRun) measure(d float64, short bool) (wizStep, error) {
	for {
		set, retune := bosEventsSince(r.since)
		if set != "" && retune == "" { // between steps: nothing measured, nothing to throw out
			r.live("bosminer moved the voltage between steps - waiting for a settled stage-4 round")
			if err := r.settled(time.Now()); err != nil {
				return wizStep{}, err
			}
			continue
		}
		var st wizStep
		if retune == "" {
			var err error
			if st, err = r.hold(d, short); err != nil {
				return st, err
			}
			set, retune = bosEventsSince(r.since)
		}
		if set == "" && retune == "" && st.Invalid != "" { // hold() found the hash counters reset (restart)
			set = st.Invalid
		}
		if set == "" && retune == "" {
			r.since = time.Now()
			if st.Dropped == "" && st.V > 0 { // a measured step (mean voltage): its drive point helps every later start
				addDrivePt(drivePt{W: r.load(), V: st.V, D: st.Drive, B: r.l.Boards, Src: "step", L: r.l.Index, Short: !st.Full})
			}
			wizEvent("hold", "%d W: %s", r.l.Watts, holdText(st))
			r.stepDone(st)
			return st, nil
		}
		why, what := set, "moved the voltage"
		if retune != "" {
			why, what = retune, "retuned"
		} else if set == st.Invalid {
			what = "restarted (hash counters reset)"
		}
		st.Drive, st.Invalid = r2(d), why
		r.stepDone(st)
		r.l.Steps = append(r.l.Steps, st)
		r.upd(func(x *wizLevel) { x.Steps = r.l.Steps })
		note("wizard: %d W: bosminer %s during a trimmed step (%s) - step thrown out", r.l.Watts, what, why)
		glideZero()
		if reason := touchVerdict(retune != "", r.l.Redos, r.l.Retunes); reason != "" {
			return st, fmt.Errorf("%s (last: %s)", reason, why)
		}
		r.l.Redos++
		if retune != "" {
			r.l.Retunes++
		}
		redos, retunes := r.l.Redos, r.l.Retunes
		r.upd(func(x *wizLevel) { x.Redos, x.Retunes = redos, retunes })
		r.live("bosminer %s - waiting for a settled stage-4 round, then redoing drive %.1f %%", what, d)
		if err := r.settled(time.Now()); err != nil {
			return st, err
		}
	}
}

// step: one search step at drive d (measure), recorded in the level. Returns the step (its mean voltage) and
// whether it passed. A failing step gets its soft reset from the caller (failReset) only when another step follows:
// the search's last step, pass or fail, is where the latch check starts from (latchStart).
func (r *levelRun) step(d float64, short bool) (wizStep, bool, error) {
	st, err := r.measure(d, short)
	if err != nil {
		return st, false, err
	}
	r.l.Steps = append(r.l.Steps, st)
	steps := r.l.Steps
	r.upd(func(x *wizLevel) { x.Steps = steps })
	r.sinceReset = append(r.sinceReset, len(steps)-1)
	pass := st.usable() && st.Pass
	if pass {
		r.bestJTH = math.Min(r.bestJTH, st.JTH)
	}
	return st, pass, nil
}

// failReset: the soft reset after a failing step, before the next step (owner 2026-11-25).
func (r *levelRun) failReset(d float64) error {
	r.live("drive %.1f %% failed - soft reset before the next step", d)
	if err := r.clean(nil); err != nil && fatal(err) {
		return err
	}
	r.l.Resets++
	resets := r.l.Resets
	r.upd(func(x *wizLevel) { x.Resets = resets })
	return nil
}

// Latch check of the answer (owner decision 2026-11-25, the cheap rule; latching only gets worse at lower voltage =
// more drive). sinceReset = indices of the steps measured since the last soft reset, in order.
//
//	(a) the last step ran at or below the answer's voltage (drive >= the answer's): straight back to stock and
//	    check. Clean -> the answer is cleared. Latched and the last step WAS the answer (same drive) -> the answer
//	    latches (raise fallback). Latched and the last step was below the answer -> inconclusive -> (b) with a reset.
//	(b) otherwise (the last step ran above the answer's voltage, or a reset came after it): a soft reset first if a
//	    step below the answer ran since the last reset (or after an inconclusive check), then latchHoldMin at the
//	    answer's drive, back to stock, check.
//
// latchStart: direct = path (a); reset = (b) needs a soft reset first; last = the last step's index (-1 = none
// since the last reset).
func latchStart(steps []wizStep, sinceReset []int, answer int) (direct, reset bool, last int) {
	const eps = 1e-6
	if len(sinceReset) == 0 {
		return false, false, -1
	}
	last = sinceReset[len(sinceReset)-1]
	ad := steps[answer].Drive
	if steps[last].Drive >= ad-eps {
		return true, false, last
	}
	for _, i := range sinceReset {
		if steps[i].Drive > ad+eps {
			reset = true
		}
	}
	return false, reset, last
}

// latchVerdict: path (a)'s result. bad = latched chips or a board down; below = the last step ran below the answer
// (more drive). "cleared" | "latches" (raise fallback) | "inconclusive" (path b with a reset).
func latchVerdict(bad, below bool) string {
	switch {
	case !bad:
		return "cleared"
	case below:
		return "inconclusive"
	}
	return "latches"
}

// latchHoldMin: the answer's hold before a latch check on path (b).
const latchHoldMin = holdMin

// latchAnswer runs the latch check of the step at index ai (latchStart / latchVerdict) and records the path taken
// in the level (LatchPath) and the event list. Returns what latchCheck returns.
func (r *levelRun) latchAnswer(ai int) (bool, []string, bool, error) {
	direct, reset, last := latchStart(r.l.Steps, r.sinceReset, ai)
	ad := r.l.Steps[ai].Drive
	path := ""
	record := func(s string) {
		path = strings.TrimPrefix(path+"; "+s, "; ")
		p := path
		r.l.LatchPath = p
		r.upd(func(x *wizLevel) { x.LatchPath = p })
		wizEvent("latch", "%d W: %s", r.l.Watts, s)
		r.latchDone(s)
	}
	result := func(known bool, latched []string, down bool) string {
		switch {
		case down:
			return "a board down"
		case len(latched) > 0:
			return "latched (" + strings.Join(latched, ", ") + ")"
		case !known:
			return "no per-chip data"
		}
		return "clean"
	}
	if direct {
		r.latchPhase(true)
		ls := r.l.Steps[last]
		known, latched, down, err := r.latchCheck()
		if err != nil {
			return known, latched, down, err
		}
		below := ls.Drive > ad+1e-6
		v := latchVerdict(len(latched) > 0 || down, below)
		what := map[string]string{"cleared": "answer cleared", "latches": "the answer latches",
			"inconclusive": "inconclusive (the last step ran below the answer): reset, 5 min at the answer"}[v]
		if !known && !down {
			what = "no per-chip data"
		}
		record(fmt.Sprintf("latch check from the last step (%s, drive %.1f %%): %s -> %s", vAvgText(ls.V), ls.Drive, result(known, latched, down), what))
		if v != "inconclusive" {
			return known, latched, down, nil
		}
		reset = true
	}
	if reset {
		if err := r.clean(nil); err != nil && fatal(err) {
			return false, nil, false, err
		}
		r.l.Resets++
		resets := r.l.Resets
		r.upd(func(x *wizLevel) { x.Resets = resets })
	}
	r.live("latch check: %d min at the answer (drive %.1f %%) first", latchHoldMin, ad)
	r.latchPhase(false)
	if err := glide(ad, wizSleep, func() bool { return dropCheck(read(), r.base) != nil }); err != nil && !errors.Is(err, errAborted) {
		if _, ok := err.(errBoardDrop); !ok {
			return false, nil, false, err
		}
	}
	if _, _, _, _, err := avgRates(latchHoldMin*time.Minute, r.base); err != nil && !counterReset(err) {
		if _, ok := err.(errBoardDrop); !ok {
			return false, nil, false, err
		}
	}
	known, latched, down, err := r.latchCheck()
	if err != nil {
		return known, latched, down, err
	}
	how := "latch check"
	if reset {
		how += ": reset"
	} else {
		how += ":"
	}
	verdict := "answer cleared"
	if len(latched) > 0 || down {
		verdict = "the answer latches"
	} else if !known {
		verdict = "no per-chip data"
	}
	record(fmt.Sprintf("%s %d min at the answer (drive %.1f %%), back to stock: %s -> %s", how, latchHoldMin, ad, result(known, latched, down), verdict))
	return known, latched, down, nil
}

// latchCheck: back at stock voltage (glide), latchMin minutes: chips weak now that were healthy in the fresh tune
// are latched; a board below 60 % counts too. Returns per-chip data known, latched chips, a board down.
func (r *levelRun) latchCheck() (bool, []string, bool, error) {
	glideZero()
	r.live("latch check at stock voltage (%d min)", latchMin)
	if !wizSleep(20 * time.Second) {
		return false, nil, false, wizStopped{}
	}
	if _, _, _, _, err := avgRates(latchMin*time.Minute, nil); err != nil && !counterReset(err) { // per-chip verdict only
		return false, nil, false, err
	}
	known, latched := false, []string(nil)
	if r.baseWeak != nil && lastChips != nil {
		known, latched = true, newWeakList(weakChips(lastChips, lastFreqs, weakFr), r.baseWeak)
	}
	down := boardDropped(read().rates, r.base, dropFr) != ""
	wizEvent("latch", "%d W: latch check at stock: %s", r.l.Watts, latchText(known, latched, down))
	return known, latched, down, nil
}

// answer: one passing step as the level's answer.
func (r *levelRun) answer(i int) *wizAnswer {
	s := r.l.Steps[i]
	return &wizAnswer{V: s.V, Drive: s.Drive, EstW: math.Round(estWallF(r.t0, s.V, 1)), THs: s.THs, JTH: s.JTH,
		NewWeak: append([]string{}, s.Weak...), Step: i}
}

// searchLevel runs one level (estimated wall watts, board set): soft reset -> fresh Braiins tune (the baseline,
// measured during the settled-round wait) ->
// gallop + bisection (search.go; every step judged as before, a soft reset after each failing step) -> answer =
// the passing step with the best est. J/TH -> latch check (latchAnswer; latched: soft reset, +latchRaise as drive, measure again) -> clocks scaled to land
// on the watts. Fans: the live fan rule throughout; the level's fan is the rule's fan at the end.
func searchLevel(lv wizLevel, in wizInputs, curves []wizCurve, upd func(func(*wizLevel)), prev *prevRes) (wizLevel, error) {
	W := float64(lv.Watts)
	l := lv
	r := &levelRun{in: in, l: &l, upd: upd, curves: curves, scale: 1}
	mt, _ := minPowerTarget()
	t0, raised := clampTarget(targetFor(W, refVolt(W)), mt) // first guess: the reference miner's best voltage
	r.t0 = t0
	if raised {
		l.MinTarget = mt
	}

	// 1. clean start: nothing carries over from the previous level (latched chips, trim). Same board set: the
	// level's target is written into bosminer.toml while bosminer is stopped, so it restarts directly at it (no
	// fresh tune of the previous target first). A board switch needs the API: the change below does it.
	r.phase("clean", "Clean reset", cleanMin*time.Minute)
	r.live("clean start: soft reset (pause, cool, bosminer off %s with trim 0, on, resume)", cleanOff)
	rd := read()
	cur := rd.target
	if cur <= 0 {
		cur = t0
	}
	boards := lv.Boards
	if boards == "" {
		boards = "123"
	}
	direct := rd.ok && rd.enabled == boards
	gate := cur
	tChange := time.Now()
	var act func() error
	var tomlErr error
	if direct {
		gate = min(cur, t0) // the hashing gate holds at either target
		act = func() error {
			tChange = time.Now() // bosminer is stopped: every round from here on is at the new target
			if tomlErr = writeAutotuneTarget(bosTomlFile, t0); tomlErr != nil {
				note("wizard: %d W: bosminer.toml not edited (%v) - the target is changed through the API", lv.Watts, tomlErr)
			}
			return nil // a failed edit is not a failed reset: the API change below covers it
		}
	}
	if err := cleanApply(0, gate, cleanOff, act); err != nil {
		if _, stopped := err.(wizStopped); stopped {
			return l, err
		}
		if err := recoverBoards(0, gate); err != nil {
			return l, err
		}
	}
	if direct = direct && tomlErr == nil; direct {
		if got := configuredTarget(); got != t0 {
			note("wizard: %d W: bosminer restarted at %d W, not %d W - changing the target through the API", lv.Watts, got, t0)
			direct = false
		}
	}

	// 2. fresh Braiins tune = the baseline
	r.phase("tune", fmt.Sprintf("Braiins tunes %d W", t0), braiinsMin*time.Minute)
	r.live("Braiins target %d W, stock voltage, fresh tune", t0)
	if direct {
		setPhase("wizard")
	} else {
		tChange = time.Now()
		if err := wizChange(t0, lv.Boards, 0); err != nil { // a board down is recovered before any baseline
			return l, err
		}
	}
	// (a) no trimming before Braiins has settled: a stage-4 round with no "Setting voltage" since the round before;
	// the stock baseline is measured during that wait (sliding window, restarted by any voltage change)
	base, th0, chip0, vs, err := r.baseline(tChange)
	if err != nil {
		return l, err
	}
	r.since = time.Now() // the guard window starts after the stock baseline (stock-voltage settling there is discarded)
	if r.snap == "" {    // bosminer may save the fresh tune a little after stage 4
		r.snap, _ = profileSnapshot(t0, len(lv.Boards))
	}
	r.base, r.baseTH = base, th0
	l.Target, l.StockV, l.StockTH, l.ChipC, l.StockRates = t0, vs, th0, chip0, base
	l.ChipSrc, l.RateSrc = lastChipSrc, lastRateSrc
	addChipSnap(lv.Index, "stock", vs, true)
	if lastChips != nil {
		r.baseChip, r.baseSrc = lastChips, lastChipSrc
		r.baseWeak = weakChips(lastChips, lastFreqs, weakFr)
		l.StockWeak = newWeakList(r.baseWeak, nil)
	}
	l.StockW = math.Round(estWall(t0, vs))
	l.StockJT = l.StockW / math.Max(0.1, th0)
	if raised {
		l.Note = fmt.Sprintf("power target raised to Braiins' minimum %d W", mt)
	}
	upd(func(x *wizLevel) { *x = l })
	if chip0 > tooHotLine(in.ChipTarget, braiinsHotTemp()) && fanNow(0) >= in.MaxFan { // stock runs hotter than trimmed (+9), capped below Braiins' hot_temp
		l.Status, l.Note = "too_hot", fmt.Sprintf("chips %.0f °C at stock with fans at %d %%", chip0, in.MaxFan)
		return l, nil
	}

	// 3. gallop + bisection in drive space (search.go): first step at the predicted voltage's drive
	floor := wizSnap().Floor
	vp, stepV := refVolt(W), gallopStep1 // predicted best voltage: the reference miner...
	if prev != nil && prev.V > 0 {       // ...or better, the last resolved level plus the reference miner's step between the two
		vp, stepV = prev.V+(refVolt(W)-refVolt(prev.W)), gallopStepNext
	}
	l.Pred = r2(vp)
	lowV := r2(math.Max(floor+0.15, W/ccfg.MaxAmps))
	plan := &drivePlan{Step: stepV, LowV: lowV, DriveAt: r.drive,
		MinD: r2(math.Max(0, r.drive(vs-stockGap))), MaxD: r2(math.Min(ccfg.TrimCap, r.drive(lowV)))}
	shortD := r.drive(vp + shortAbove) // less drive than this = a short check
	r.bestJTH = l.StockJT
	if plan.MaxD > plan.MinD {
		d := plan.first(r.drive(vp))
		for {
			short := d < shortD-1e-6
			r.liveRaw(fmt.Sprintf("%d W · step %d · testing ≈ %.2f V", lv.Watts, plan.N+1, voltAtDrive(r.drive, d)))
			st, pass, err := r.step(d, short)
			if err != nil {
				glideZero()
				return l, err
			}
			wizEvent("step", "%d W: step %d: drive %.1f %% -> %s (%s)", lv.Watts, plan.N+1, d, vAvgText(st.V), map[bool]string{true: "pass", false: "fail"}[pass])
			nd, done := plan.next(d, st.V, pass)
			if done {
				break // a failing last step keeps its state: the latch check may start from it (latchStart)
			}
			if !pass {
				if err := r.failReset(d); err != nil {
					return l, err
				}
			}
			d = nd
		}
	}
	upd(func(x *wizLevel) { *x = l })

	// 4. answer = the passing step with the best est. J/TH (answerStep); latch check (latchAnswer: from the last step when it ran at
	// or below the answer's voltage, else 5 min at the answer first); latched -> soft reset, less drive by
	// latchRaise's equivalent, measure again
	ai := -1
	if plan.HasPass {
		ai = answerStep(l.Steps)
	}
	for ai >= 0 {
		l.Eff = r.answer(ai)
		known, latched, down, err := r.latchAnswer(ai)
		if err != nil {
			glideZero()
			return l, err
		}
		l.LatchKnown, l.Latched = known, latched
		if !down && effSafe(known, latched) {
			l.Eff.Safe = true
			break
		}
		if known && (len(latched) > 0 || down) {
			l.LatchedAt = append(l.LatchedAt, l.Eff.V)
		}
		if len(latched) > 0 || down {
			note("wizard: %d W: answer %.2f V (drive %.1f %%) latched %v, board down %v - soft reset", lv.Watts, l.Eff.V, l.Eff.Drive, latched, down)
			if err := r.clean(nil); err != nil && fatal(err) {
				return l, err
			}
		}
		if !known || l.Raises >= maxRaises { // no per-chip data, or the raise cap
			break
		}
		l.Raises++
		d, v := l.Eff.Drive, l.Eff.V
		d = r2(d - driveStep(r.drive, v, latchRaise))
		ai = -1
		for d > plan.MinD-1e-6 && ai < 0 { // a raise that fails its step raises again (same cap)
			r.live("latched: re-measuring drive %.1f %% (raise %d of %d)", d, l.Raises, maxRaises)
			st, pass, err := r.step(d, false)
			if err != nil {
				glideZero()
				return l, err
			}
			if pass {
				ai = len(l.Steps) - 1
			} else if l.Raises >= maxRaises {
				break
			} else {
				if err := r.failReset(d); err != nil {
					return l, err
				}
				l.Raises++
				if st.V > 0 {
					v = st.V
				}
				d = r2(d - driveStep(r.drive, v, latchRaise))
			}
		}
		upd(func(x *wizLevel) { *x = l })
	}
	if l.Eff != nil && !l.Eff.Safe {
		ai = -1
	}
	if ai < 0 { // no safe trimmed answer: the level runs at stock voltage
		l.V, l.EstW, l.THs = 0, math.Round(estWall(t0, vs)), th0
		l.EstJTH = l.EstW / math.Max(0.1, th0)
		l.Status, l.Answer = "done", "stock"
		why := "no passing trimmed voltage"
		switch {
		case len(l.LatchedAt) > 0:
			l.Status, why = "unsafe", latchedAtNote(l.LatchedAt)
		case l.Eff != nil && !l.LatchKnown:
			l.Status, why = "unsafe", "no per-chip data for the latch check"
		}
		l.Note = strings.TrimPrefix(l.Note+"; "+why+": stock voltage", "; ")
		glideZero()
		l.Fan = fanNow(in.MaxFan)
		r.profileEnd()
		return l, nil
	}
	chosen := l.Eff
	l.Answer = "efficiency"
	if len(l.LatchedAt) > 0 {
		l.Note = strings.TrimPrefix(l.Note+"; "+latchedAtNote(l.LatchedAt)+fmt.Sprintf(": raised to %.2f V", chosen.V), "; ")
	}
	// the answer's drive and mean voltage; a margin = less drive by its equivalent at the local slope
	l.V, l.THs, l.Drive = r2(chosen.V+in.MarginV), chosen.THs, chosen.Drive
	if in.MarginV > 0 {
		l.Drive = r2(math.Max(0, chosen.Drive-driveEq(r.drive, chosen.V, in.MarginV)))
	}
	upd(func(x *wizLevel) { *x = l })

	// 5. land on the round wattage by scaling clocks (never a new Braiins tune): soft reset with the clocks written
	// into the saved stage-4 entry, then re-confirm the answer (one confirmUp step up if higher clocks need it)
	f := 1.0
	if est := estWall(t0, l.V); math.Abs(est-W) > 25 {
		f = (W - powOverhead) / (powGain * float64(t0) * math.Pow(l.V/powRefV, powExp))
		f = math.Max(0.85, math.Min(1.15, f))
		nb := len(lv.Boards)
		orig, oerr := profileEntriesSaved(t0, nb) // to put the clocks back if the scaled ones do not hold
		r.live("lands at ≈%.0f W - scaling clocks x%.3f with a soft reset", est, f)
		if oerr != nil {
			l.Note = strings.TrimPrefix(l.Note+"; clock correction skipped (saved tune unreadable: "+oerr.Error()+")", "; ")
			f = 1
		} else if err := r.clean(func() error { _, err := scaleProfileClocks(t0, nb, f); return err }); err != nil {
			if fatal(err) {
				return l, err
			}
			l.Note = strings.TrimPrefix(l.Note+"; clock correction failed ("+err.Error()+"); level kept at its first clocks", "; ")
			f = 1
		} else {
			r.scale = f
			r.snap, _ = profileSnapshot(t0, nb) // our own change: the guard compares from here
			holds := func(st wizStep) bool { return st.usable() && st.JTH <= chosen.JTH*effConfirmTo }
			ok, cerr := false, error(nil) // cerr: the confirm itself failed (level fails, clocks restored first)
			for try, dd := 0, l.Drive; try < 2 && !ok; try, dd = try+1, r2(math.Max(0, dd-driveStep(r.drive, l.V, confirmUp))) {
				st, err := r.measure(dd, false)
				if err != nil {
					cerr = err
					break
				}
				l.Steps = append(l.Steps, st)
				upd(func(x *wizLevel) { x.Steps = l.Steps })
				if ok = holds(st); ok {
					l.V, l.THs, l.Drive = st.V, st.THs, st.Drive
				} else if try == 0 {
					if err := r.clean(nil); err != nil && fatal(err) {
						cerr = err
						break
					}
				}
			}
			if fatal(cerr) {
				glideZero()
				return l, cerr
			}
			if !ok { // never leave scaled clocks saved for a level they did not hold at: put the originals back
				r.live("clocks x%.3f did not hold - restoring the original clocks (soft reset)", f)
				if err := r.clean(func() error { return restoreProfileEntries(orig) }); err != nil {
					if fatal(err) {
						return l, err
					}
					return l, fmt.Errorf("clocks x%.3f did not hold and the original clocks could not be restored: %v", f, err)
				}
				r.scale = 1
				r.snap, _ = profileSnapshot(t0, nb)
				l.Note = strings.TrimPrefix(l.Note+fmt.Sprintf("; clocks x%.3f did not hold: original clocks restored (lands at ≈%.0f W)", f, est), "; ")
				f = 1
				if cerr != nil {
					return l, cerr
				}
			}
		}
	}
	glideZero()
	l.Clock = f
	l.EstW = math.Round(estWallF(t0, l.V, f))
	l.EstJTH = l.EstW / math.Max(0.1, l.THs)
	l.Fan = fanNow(in.MaxFan) // the live fan rule's fan at the end of the level: its default fan
	r.profileEnd()
	l.Status = "done"
	return l, nil
}

// dText: a drive for the live line ("–" = none yet).
func dText(d float64, ok bool) string {
	if !ok {
		return "–"
	}
	return fmt.Sprintf("%.1f %%", d)
}

// vAvgText: a step's mean voltage for the event list ("? V avg" = unknown, e.g. an aborted hold).
func vAvgText(v float64) string {
	if v <= 0 {
		return "? V avg"
	}
	return fmt.Sprintf("%.2f V avg", v)
}

// profileEnd: last guard checks of a level — a touch after the last step, and Braiins' saved tune for the target
// compared with the snapshot taken after the fresh tune (or after our own clock write).
func (r *levelRun) profileEnd() {
	if _, retune := bosEventsSince(r.since); retune != "" { // stage-4 voltage settling is not reported
		r.l.ProfileNote = "bosminer retuned after the last step: " + retune
	}
	now, err := profileSnapshot(r.t0, len(r.l.Boards))
	switch {
	case err != nil:
		r.l.ProfileNote = strings.TrimPrefix(r.l.ProfileNote+"; saved tunes unreadable: "+err.Error(), "; ")
	case r.snap == "" && now == "":
		r.l.ProfileNote = strings.TrimPrefix(r.l.ProfileNote+"; no saved tune for this target to guard", "; ")
	case now != r.snap:
		r.l.ProfileChanged = true
		note("wizard: %d W: Braiins' saved tune for %d W changed during the level", r.l.Watts, r.t0)
	}
}

func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

// rankBoards: strongest -> weakest at low voltage = the board that stayed at its rate down to the lowest voltage
// (the lowest step where it kept rankFr of its stock rate; steps are not in voltage order with gallop + bisection).
func rankBoards(l wizLevel) []string {
	if len(l.Steps) == 0 {
		return nil
	}
	minV := map[string]float64{}
	for _, st := range l.Steps { // every board seen in any step (the first step may be an aborted hold)
		for b := range st.Shares {
			minV[b] = l.StockV
		}
	}
	for b := range minV {
		for _, st := range l.Steps {
			if st.Invalid != "" {
				continue // thrown out by the profile guard
			}
			if st.Shares[b] >= rankFr && st.V < minV[b] {
				minV[b] = st.V
			}
		}
	}
	out := []string{}
	for b := range minV {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if minV[out[i]] != minV[out[j]] {
			return minV[out[i]] < minV[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// GET /api/wizard/chips[?level=N]: without level, which levels have chip data; with it, that level's snapshots.
func handleWizardChips(w http.ResponseWriter, r *http.Request) {
	chipMu.Lock()
	defer chipMu.Unlock()
	if q := r.URL.Query().Get("level"); q != "" {
		n, _ := strconv.Atoi(q)
		jsonOut(w, map[string]interface{}{"level": n, "snaps": wizChips[n]})
		return
	}
	idx := []int{}
	for k := range wizChips {
		idx = append(idx, k)
	}
	sort.Ints(idx)
	jsonOut(w, map[string]interface{}{"levels": idx})
}

var listenMu sync.Mutex

func handleWizard(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s := wizSnap()
		coreMu.Lock()
		ph := phase
		coreMu.Unlock()
		tv := tunerNow()
		step := s.Live
		if s.Running && tv.Wait != "" { // waiting for Braiins: the live line says what Braiins is doing
			s.Live = tv.Line
		}
		out := map[string]interface{}{"state": s, "table": wizTable(s), "fits": driveFits(s), "phase": ph, "tuner": tv, "live_step": step,
			"resume":   "",
			"defaults": wizInputs{MinW: 500, MaxW: 2200, StepW: 100, MaxFan: 25, ChipTarget: 74},
			"limits":   map[string]int{"min_fan": minFan, "two_board_below": twoBoardW, "one_board_below": oneBoardW}}
		if c := calSnapshot(); c.Running && len(c.Points) > 0 { // the sweep in progress: the page draws it as it goes
			out["sweep_live"] = wizCurve{Watts: c.Watts, StockV: c.StockV, Points: c.Points, Status: "running"}
		}
		if ok, _ := resumable(s); ok {
			out["resume"] = resumeSummary(s)
		}
		if q := r.URL.Query(); q.Get("min_w") != "" {
			in := wizInputs{}
			in.MinW, _ = strconv.Atoi(q.Get("min_w"))
			in.MaxW, _ = strconv.Atoi(q.Get("max_w"))
			in.StepW, _ = strconv.Atoi(q.Get("step_w"))
			in.FewerBoards = q.Get("fewer") == "1"
			in.MaxFan, _ = strconv.Atoi(q.Get("max_fan"))
			if in.StepW > 0 && in.MaxW >= in.MinW {
				if len(s.Stages) > 0 && !s.Running { // what "Tune the added levels" would run
					ct, _ := strconv.ParseFloat(q.Get("chip"), 64)
					xin := s.Inputs
					xin.MinW, xin.MaxW, xin.StepW, xin.FewerBoards, xin.MaxFan, xin.ChipTarget = in.MinW, in.MaxW, in.StepW, in.FewerBoards, in.MaxFan, ct
					if x, err := extendPlan(s, xin); err != nil {
						out["extend"] = map[string]interface{}{"ok": false, "reason": err.Error()}
					} else {
						out["extend"] = map[string]interface{}{"ok": x.Added > 0, "text": extendText(x, levelMin(false)), "added": x.Added,
							"kept": x.Kept, "outside": x.Outside, "reason": map[bool]string{true: "", false: "no level to add in this range"}[x.Added > 0]}
					}
				}
				out["estimate_min"] = estMinutes(in)
				out["levels"] = len(in.levels())
				out["level_min"] = levelMin(true)
				out["level_min_next"] = levelMin(false)
				out["level_max_min"] = levelMaxMin()
			}
		}
		jsonOut(w, out)
		return
	}
	if !authOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		jsonOut(w, map[string]interface{}{"ok": false, "error": "bad password"})
		return
	}
	var body struct {
		Action   string     `json:"action"`
		Fan      int        `json:"fan"`
		TrimLive bool       `json:"trim_live"`
		Levels   []levelKey `json:"levels"`    // clear_levels
		RedoPrep bool       `json:"redo_prep"` // resume: preparation runs again (resume.go resumePrep)
		wizInputs
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonOut(w, map[string]interface{}{"ok": false, "error": "bad json"})
		return
	}
	bad := func(e string) { jsonOut(w, map[string]interface{}{"ok": false, "error": e}) }
	switch body.Action {
	case "listen":
		if refuseDuringSetup(w) { // the setup owns the fans
			return
		}
		if body.Fan < minFan || body.Fan > 100 {
			bad(fmt.Sprintf("fan must be %d..100 %%", minFan))
			return
		}
		if !listenMu.TryLock() {
			bad("a listen test is already running")
			return
		}
		prev, _ := fanPct().(int)
		if out, err := fanSet(body.Fan); err != nil {
			listenMu.Unlock()
			bad(fmt.Sprintf("fan: %v %s", err, out))
			return
		}
		go func() {
			defer listenMu.Unlock()
			time.Sleep(30 * time.Second)
			if prev >= minFan {
				_, _ = fanSet(prev)
			}
		}()
		jsonOut(w, map[string]interface{}{"ok": true, "back_to": prev})
	case "stop":
		requestStop()
		jsonOut(w, map[string]interface{}{"ok": true})
	case "start":
		in := body.wizInputs
		switch {
		case drv == nil:
			bad("no trim output on this control board")
			return
		case in.MinW < 300 || in.MaxW > 3000 || in.MinW >= in.MaxW || in.StepW < 50:
			bad("range must be 300..3000 W with min < max and step ≥ 50 W")
			return
		case in.MaxFan < minFan || in.MaxFan > 100:
			bad(fmt.Sprintf("maximum fan must be %d..100 %%", minFan))
			return
		case !in.ResetTunes:
			bad("setup needs the saved Braiins tunes moved aside first (tick the acknowledgement)")
			return
		case in.ChipTarget < 55 || in.ChipTarget > 80:
			bad("chip temperature target must be 55..80 °C")
			return
		case in.MarginV < 0 || in.MarginV > 0.2:
			bad("voltage margin must be 0..0.2 V")
			return
		}
		if ok, busy := setupBegin("wizard", "wizard"); !ok {
			bad("core busy (" + busy + ")")
			return
		}
		est := estMinutes(in) // setupBegin cleared any stale stop
		st := []wizStage{}
		for _, t := range wizStageTitles {
			st = append(st, wizStage{Key: t[0], Title: t[1], Status: "waiting", EstMin: est[t[0]]})
		}
		mr := wizSnap().ManualRestore // an earlier failed restore is still owed
		wizSet(func(s *wizState) {
			*s = wizState{Running: true, Inputs: in, Stages: st, Started: time.Now().Unix(), ManualRestore: mr}
		})
		chipMu.Lock()
		wizChips = map[int][]chipSnap{}
		chipMu.Unlock()
		_ = os.Remove(chipFile())
		note("wizard: start %d-%d W step %d, max fan %d %%, chip target %.0f °C, fewer boards %v, margin %.2f V",
			in.MinW, in.MaxW, in.StepW, in.MaxFan, in.ChipTarget, in.FewerBoards, in.MarginV)
		go wizRun(in, false, false)
		jsonOut(w, map[string]interface{}{"ok": true})
	case "resume":
		s := wizSnap()
		if ok, why := resumable(s); !ok {
			bad("nothing to resume: " + why)
			return
		}
		if drv == nil {
			bad("no trim output on this control board")
			return
		}
		if ok, busy := setupBegin("wizard", "wizard"); !ok {
			bad("core busy (" + busy + ")")
			return
		}
		var reset []int
		wizSet(func(s *wizState) { reset = prepareResume(s, time.Now().Unix()) })
		chipMu.Lock()
		for _, i := range reset {
			delete(wizChips, i)
		}
		chipMu.Unlock()
		if body.RedoPrep {
			note("wizard: resume, preparation redone (%s)", resumeSummary(s))
		} else {
			note("wizard: resume (%s)", resumeSummary(s))
		}
		go wizRun(s.Inputs, true, body.RedoPrep)
		jsonOut(w, map[string]interface{}{"ok": true})
	case "extend": // resume with a new range (extend.go); max fan and chip target stay the saved run's
		s := wizSnap()
		in := s.Inputs
		in.MinW, in.MaxW, in.StepW, in.FewerBoards = body.MinW, body.MaxW, body.StepW, body.FewerBoards
		if body.MaxFan != 0 || body.ChipTarget != 0 {
			in.MaxFan, in.ChipTarget = body.MaxFan, body.ChipTarget
		}
		switch {
		case drv == nil:
			bad("no trim output on this control board")
			return
		case in.MinW < 300 || in.MaxW > 3000 || in.MinW >= in.MaxW || in.StepW < 50:
			bad("range must be 300..3000 W with min < max and step ≥ 50 W")
			return
		}
		x, err := extendPlan(s, in)
		if err != nil {
			bad(err.Error())
			return
		}
		if ok, busy := setupBegin("wizard", "wizard"); !ok {
			bad("core busy (" + busy + ")")
			return
		}
		var reset []int
		wizSet(func(s *wizState) {
			applyExtend(s, in, x)
			reset = prepareResume(s, time.Now().Unix())
		})
		chipMu.Lock()
		wizChips = remapChips(wizChips, x.Remap)
		for _, i := range reset {
			delete(wizChips, i)
		}
		b, _ := json.Marshal(wizChips)
		chipMu.Unlock()
		_ = os.WriteFile(chipFile(), b, 0o644)
		note("wizard: extend to %d-%d W step %d (fewer boards %v): %s", in.MinW, in.MaxW, in.StepW, in.FewerBoards, extendText(x, levelMin(false)))
		go wizRun(wizSnap().Inputs, true, false)
		jsonOut(w, map[string]interface{}{"ok": true})
	case "clear_levels", "clear_all": // a saved run only; never production's table or Braiins' tunes / backups
		if why := clearRefused(wizSnap(), setupRunning()); why != "" {
			bad(why)
			return
		}
		var cleared []int
		wizSet(func(s *wizState) {
			if body.Action == "clear_all" {
				clearAll(s)
			} else {
				cleared = clearLevels(s, body.Levels, time.Now())
			}
		})
		chipMu.Lock()
		if body.Action == "clear_all" {
			wizChips = map[int][]chipSnap{}
		}
		for _, i := range cleared {
			delete(wizChips, i)
		}
		b, _ := json.Marshal(wizChips)
		chipMu.Unlock()
		_ = os.WriteFile(chipFile(), b, 0o644)
		note("wizard: %s (%d levels)", body.Action, len(cleared))
		jsonOut(w, map[string]interface{}{"ok": true, "cleared": len(cleared)})
	case "apply": // kept for scripts: the table is written automatically as each level finishes (applyWizTable)
		n, err := applyWizTable()
		if err != nil {
			bad(err.Error())
			return
		}
		jsonOut(w, map[string]interface{}{"ok": true, "levels": n})
	default:
		bad("action must be start, resume, extend, clear_levels, clear_all, stop, listen or apply")
	}
}
