package main

// calib.go — trim calibration sweep, run from the web UI.
//
// At the level the miner is running now: glide the trim to `start`, then step by `step` % up to
// `stop`, recording the mean and per-board voltage (several reads per step) and the board rates.
// Stops early when any board's rate falls below hash_limit × its own starting rate (unless keep_going: the setup
// wizard's sweep always runs to the end and only flags those boards per point), or on request; always
// glides back to 0 % (stock voltage) at the end. One curve per load (low / mid / high wattage) is
// enough: the level table's starting drive values are fitted from them.
//
//	GET  /api/calibrate              current/last run + saved runs
//	GET  /api/calibrate?file=NAME    one saved run
//	POST /api/calibrate {"action":"start","start":30,"stop":75,"step":0.25,"settle":6,"reads":3,"hash_limit":0.6}
//	POST /api/calibrate {"action":"stop"}                                     (both X-Auth)
//
// Runs only when the core is idle (no level change in progress) and holds the core's phase as
// "calibrate"; while it runs outside control is refused and the core's loops stand down (guard.go).
// Every trim move is at most the setup pace (5 % per 10 s). Results: /etc/s19volt/calib/.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const calibDir = "/etc/s19volt/calib"

type calPoint struct {
	Duty    float64            `json:"duty"`
	V       float64            `json:"v"`                 // mean of the reads (mean of the boards with a voltage: read() drops a board reading ~0 V)
	BoardV  map[string]float64 `json:"board_v,omitempty"` // per board, mean of the reads
	Spread  float64            `json:"spread"`            // max-min of the reads
	MinRate float64            `json:"min_rate"`
	Rates   map[string]float64 `json:"rates"`
	// boards below hash_limit × their starting rate at this point (sorted; every board when nothing hashes)
	NotHashing []string `json:"not_hashing,omitempty"`
	T          int64    `json:"t"`
}

type calRun struct {
	Running   bool               `json:"running"`
	Watts     int                `json:"watts"`
	Boards    string             `json:"boards"`
	Start     float64            `json:"start"`
	Stop      float64            `json:"stop"`
	Step      float64            `json:"step"`
	Settle    float64            `json:"settle"`
	Reads     int                `json:"reads"`
	HashLimit float64            `json:"hash_limit"`
	StockV    float64            `json:"stock_v"`
	BaseRate  float64            `json:"base_rate"`
	BaseRates map[string]float64 `json:"base_rates,omitempty"` // per board at the start (the hash_limit reference)
	Label     string             `json:"label,omitempty"`      // what the miner runs (event text; default "<target> W")
	// KeepGoing: boards below hash_limit do not stop the sweep (flagged per point only). The setup wizard's sweep:
	// the floor belongs to the PSU + series resistor, not the boards, and a board dropping out at the test clock
	// says nothing about the low-clock levels.
	KeepGoing bool `json:"keep_going,omitempty"`
	// DropTrim: at the end the trim goes to 0 at once instead of gliding back (the setup wizard: bosminer restarts
	// next anyway, and more voltage at the 250 MHz test clocks is harmless)
	DropTrim bool `json:"-"`
	// Mod check (setup wizard): ModDV > 0 -> the first point at least ModDV below the 0 % point proves the mod acts
	// (ModAt/ModV, OnMod called once at that moment); no such point by ModBy % -> the sweep stops, ModFailed.
	ModDV     float64                      `json:"mod_dv,omitempty"`
	ModBy     float64                      `json:"mod_by,omitempty"`
	ModAt     float64                      `json:"mod_at,omitempty"`
	ModV      float64                      `json:"mod_v,omitempty"`
	ModFailed bool                         `json:"mod_failed,omitempty"`
	OnMod     func(stock, v, duty float64) `json:"-"`
	Points    []calPoint                   `json:"points"`
	Status    string                       `json:"status"`
	Started   int64                        `json:"started"`
	Ended     int64                        `json:"ended"`
	File      string                       `json:"file"`
}

var (
	calMu sync.Mutex
	cal   calRun
)

func calSet(f func(c *calRun)) { calMu.Lock(); f(&cal); calMu.Unlock() }

func calSnapshot() calRun {
	calMu.Lock()
	defer calMu.Unlock()
	c := cal
	c.Points = append([]calPoint(nil), cal.Points...)
	return c
}

// calStopped: a stop was requested (the shared setup stop flag, guard.go; it stays set until the next run).
func calStopped() bool { return stopRequested() }

// calSleep waits d, returning false early if a stop was requested.
func calSleep(d time.Duration) bool { return stoppableSleep(d) }

func runCalibration(c calRun) {
	defer setupEnd()                // last: outside control and the core's loops come back after the glide to 0
	defer recoverSetup("calibrate") // a panic: trim to 0, resume
	sweepCurve(c)
}

// sweepCurve runs one sweep (the setup wizard's trim sweep and the calibration card's) synchronously at the level the miner is running now, publishing progress
// in the shared calibration state (the UI card shows it live), saves it, and returns the final run.
// The caller owns the core phase (the card's handler or the setup wizard).
func sweepCurve(c calRun) calRun {
	c.Running, c.Status, c.Started, c.Points, c.File, c.Ended = true, "starting", time.Now().Unix(), nil, "", 0
	calSet(func(x *calRun) { *x = c })
	runSweep(c)
	return calSnapshot()
}

func runSweep(c calRun) {
	finish := func(status string) {
		if c.DropTrim { // the wizard restarts bosminer right after (tuner mode back): trim 0 at once, no glide
			trimOffNow()
		} else {
			glideZero() // back to stock voltage at the setup pace (5 % per 10 s)
		}
		calSet(func(x *calRun) { x.Running, x.Status, x.Ended = false, status, time.Now().Unix() })
		snap := calSnapshot()
		if status == "stopped" { // a stopped partial curve is not saved
			note("calibrate: stopped (%d points, not saved)", len(snap.Points))
			return
		}
		_ = os.MkdirAll(calibDir, 0o755)
		name := fmt.Sprintf("%dW-%s.json", snap.Watts, time.Unix(snap.Started, 0).Format("2006-01-02-1504"))
		if b, err := json.MarshalIndent(snap, "", " "); err == nil && os.WriteFile(filepath.Join(calibDir, name), b, 0o644) == nil {
			calSet(func(x *calRun) { x.File = name })
		}
		note("calibrate: %s (%d points) -> %s", status, len(snap.Points), name)
	}
	r := read()
	if !r.ok || !r.mining || r.minRate <= 0 || r.volts <= 0 {
		finish("refused: the miner is not hashing")
		return
	}
	c.Watts, c.Boards, c.StockV, c.BaseRate, c.BaseRates = r.target, r.enabled, r.volts, r.minRate, r.rates // local copy drives the stop checks
	calSet(func(x *calRun) {
		x.Watts, x.Boards, x.StockV, x.BaseRate, x.BaseRates = c.Watts, c.Boards, c.StockV, c.BaseRate, c.BaseRates
	})
	label := c.Label
	if label == "" {
		label = fmt.Sprintf("%d W", r.target)
	}
	note("calibrate: %s, %.2f-%.2f %% step %.2f, settle %.0f s, stock %.2f V", label, c.Start, c.Stop, c.Step, c.Settle, r.volts)
	if err := glide(c.Start, calSleep, nil); err != nil || !calSleep(10*time.Second) { // setup pace + settle
		finish("stopped")
		return
	}
	for d := c.Start; d <= c.Stop+1e-9; d += c.Step {
		_ = setTrim(d) // small direct steps (card: <= the setup pace, checked by the handler; wizard: 2 % per ~3 s)
		calSet(func(x *calRun) { x.Status = fmt.Sprintf("at %.2f %%", d) })
		if !calSleep(time.Duration(c.Settle * float64(time.Second))) {
			finish("stopped")
			return
		}
		var vs []float64
		var last reading
		bsum, bn := map[string]float64{}, map[string]int{}
		for i := 0; i < c.Reads; i++ {
			last = read()
			if last.ok && last.volts > 0 {
				vs = append(vs, last.volts)
				for b, v := range last.bvolts {
					bsum[b] += v
					bn[b]++
				}
			}
			if i < c.Reads-1 && !calSleep(1500*time.Millisecond) {
				finish("stopped")
				return
			}
		}
		if len(vs) == 0 {
			finish(fmt.Sprintf("stopped at %.2f %%: no voltage reading", d))
			return
		}
		mean, lo, hi := 0.0, math.Inf(1), math.Inf(-1)
		for _, v := range vs {
			mean += v
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
		mean /= float64(len(vs))
		bv := map[string]float64{}
		for b, v := range bsum {
			bv[b] = math.Round(v/float64(bn[b])*1000) / 1000
		}
		p := calPoint{Duty: math.Round(d*100) / 100, V: math.Round(mean*1000) / 1000, BoardV: bv, Spread: math.Round((hi-lo)*1000) / 1000,
			MinRate: last.minRate, Rates: last.rates, NotHashing: notHashing(last.rates, c.BaseRates, c.HashLimit, last.mining), T: time.Now().Unix()}
		calSet(func(x *calRun) { x.Points = append(x.Points, p) })
		if c.ModDV > 0 && c.ModAt == 0 {
			stock := calSnapshot().Points[0].V // the 0 % reading (Start = 0 for a mod check)
			switch modCheck(stock, p.V, p.Duty, c.ModDV, c.ModBy) {
			case modWorks:
				c.ModAt, c.ModV = p.Duty, p.V
				calSet(func(x *calRun) { x.ModAt, x.ModV = p.Duty, p.V })
				if c.OnMod != nil {
					c.OnMod(stock, p.V, p.Duty)
				}
			case modFailed:
				calSet(func(x *calRun) { x.ModFailed = true })
				finish(fmt.Sprintf("the mod is not acting on the voltage: %.2f -> %.2f V at %.0f %% (less than %.2f V)", stock, p.V, p.Duty, c.ModDV))
				return
			}
		}
		if c.KeepGoing {
			// no board stop: run to the end (the PSU current check below still applies)
		} else if b := boardDropped(last.rates, c.BaseRates, c.HashLimit); !last.mining || b != "" {
			if b == "" {
				b = "every"
			}
			finish(fmt.Sprintf("hashing limit at %.2f %% (%.2f V): board %s below %.0f %% of its rate", d, mean, b, c.HashLimit*100))
			return
		}
		if last.volts > 0 && last.volts < minVolts(last.target) {
			finish(fmt.Sprintf("PSU current limit at %.2f %% (%.2f V)", d, mean))
			return
		}
	}
	finish("done")
}

// notHashing: the boards (of ref) below fr × their reference rate, sorted; every board of ref when the miner is not
// mining at all. fr <= 0: none.
func notHashing(rates, ref map[string]float64, fr float64, mining bool) []string {
	if fr <= 0 {
		return nil
	}
	var out []string
	for b, r0 := range ref {
		if r0 > 0 && (!mining || rates[b] < fr*r0) {
			out = append(out, b)
		}
	}
	sort.Strings(out)
	return out
}

type modResult int

const (
	modUnknown modResult = iota // not yet decided: keep sweeping
	modWorks                    // the voltage dropped by at least dv below stock
	modFailed                   // reached `by` % without that drop
)

// modCheck: one sweep point (drive duty, mean board voltage v) against the 0 % reading stock.
func modCheck(stock, v, duty, dv, by float64) modResult {
	switch {
	case stock > 0 && v > 0 && stock-v >= dv-1e-9:
		return modWorks
	case duty >= by-1e-9:
		return modFailed
	}
	return modUnknown
}

func handleCalibrate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if f := r.URL.Query().Get("file"); f != "" {
			b, err := os.ReadFile(filepath.Join(calibDir, filepath.Base(f)))
			if err != nil {
				jsonOut(w, map[string]interface{}{"ok": false, "error": "no such run"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
			return
		}
		files, _ := filepath.Glob(filepath.Join(calibDir, "*.json"))
		names := []string{}
		for _, f := range files {
			names = append(names, filepath.Base(f))
		}
		sort.Sort(sort.Reverse(sort.StringSlice(names)))
		jsonOut(w, map[string]interface{}{"run": calSnapshot(), "saved": names})
		return
	}
	if !authOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		jsonOut(w, map[string]interface{}{"ok": false, "error": "bad password"})
		return
	}
	req := calRun{Start: 30, Stop: 75, Step: 0.25, Settle: 6, Reads: 3, HashLimit: 0.6}
	var body struct {
		Action string `json:"action"`
		calRun
	}
	body.calRun = req
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonOut(w, map[string]interface{}{"ok": false, "error": "bad json"})
		return
	}
	switch strings.ToLower(body.Action) {
	case "stop":
		requestStop()
		jsonOut(w, map[string]interface{}{"ok": true})
	case "start":
		c := body.calRun
		switch {
		case drv == nil:
			jsonOut(w, map[string]interface{}{"ok": false, "error": "no trim hardware"})
			return
		case c.Step < 0.1 || c.Start < 0 || c.Stop > ccfg.TrimCap || c.Start >= c.Stop || c.Reads < 1 || c.Settle < 2 || c.HashLimit <= 0 || c.HashLimit >= 1:
			jsonOut(w, map[string]interface{}{"ok": false, "error": fmt.Sprintf("bad range (0 ≤ start < stop ≤ %.0f, step ≥ 0.1, settle ≥ 2 s)", ccfg.TrimCap)})
			return
		}
		if c.Step/c.Settle > paceStep/paceDelay.Seconds() {
			jsonOut(w, map[string]interface{}{"ok": false, "error": fmt.Sprintf("step / settle must be ≤ %.0f %% per %.0f s (setup pace)", paceStep, paceDelay.Seconds())})
			return
		}
		if ok, busy := setupBegin("calibrate", "calibrate"); !ok {
			jsonOut(w, map[string]interface{}{"ok": false, "error": "core busy (" + busy + ")"})
			return
		}
		go runCalibration(c)
		jsonOut(w, map[string]interface{}{"ok": true})
	default:
		jsonOut(w, map[string]interface{}{"ok": false, "error": "action must be start or stop"})
	}
}
