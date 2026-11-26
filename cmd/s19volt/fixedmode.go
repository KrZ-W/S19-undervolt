package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// ---- fixed test profile (owner 2026-11-25) ----
// The wizard's early stage (the trim sweep: mod check, drive curve, floor) runs on ONE fixed manual profile: Braiins' tuner
// off, one global frequency and one global voltage for every chip. The user knows the mod works within minutes
// instead of after a full Braiins tune. Braiins' tuner comes back (pre-run target) right before the search.
// CONSERVATIVE clock on purpose (owner): ~250 MHz for every user's range. High clocks at low voltage latched boards
// before; a low clock leaves the trim plenty of room before chips fail.
// VOLTAGE = Braiins' MINIMUM PSU voltage (its floor; ~11.96 V on 26.11), not a "generous" value (owner): the tuned
// levels run with Braiins requesting its floor and the trim lowers the board voltage FROM that floor, so the drive
// curve (drive % -> board V) and the floor reading are only valid when measured from the same setpoint. Read from
// GET /configuration/constraints when it exposes a voltage minimum, else fixedVoltFallback.
const (
	fixedMHz          = 250.0 // MHz on every chip (~900-1000 W bosminer target on the reference miner)
	fixedVoltFallback = 11.96 // V: Braiins' minimum PSU voltage on 26.11 (when the constraints do not say)
	fixedRefMHz       = 260.0 // reference miner: target 1000 W ran ~260 MHz on all chips ("S3 it33: target 1000.00 W ... hb1 261.2 MHz")
	fixedRefTgt       = 1000.0
	fixedHashWait     = 6 * time.Minute     // cap for "every board hashing" after the switch (usually ~2 min)
	fixedModeMin      = 3                   // estimate: switch + clocks at the profile + PSU settled
	tunerBackMin      = 6                   // estimate: tuner mode back + saved tunes moved aside (bosminer restarts)
	perfModePath      = "/performance/mode" // REST path of PerformanceService/SetPerformanceMode (GET verified, PUT NOT)
)

// fixedProfile: the conservative test profile's clock and the bosminer target it corresponds to (hashing gates,
// drive-point load; scaled from the reference constant).
func fixedProfile() (target int, mhz float64) {
	return int(math.Round(fixedMHz / fixedRefMHz * fixedRefTgt)), fixedMHz
}

// parseMinVolt: a voltage minimum from a bosminer constraints reply: any object under a key containing "voltage"
// with min.volt (e.g. hashboards_constraints.voltage.min.volt; snake or camel case). The first by sorted key path;
// only plausible PSU values (5..20 V); 0 = none.
func parseMinVolt(raw json.RawMessage) float64 {
	var v interface{}
	if json.Unmarshal(raw, &v) != nil {
		return 0
	}
	var walk func(x interface{}, inVolt bool) float64
	walk = func(x interface{}, inVolt bool) float64 {
		switch t := x.(type) {
		case map[string]interface{}:
			if inVolt {
				if mn, ok := t["min"].(map[string]interface{}); ok {
					if f, ok := mn["volt"].(float64); ok && f >= 5 && f <= 20 {
						return f
					}
				}
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if f := walk(t[k], strings.Contains(strings.ToLower(k), "voltage")); f > 0 {
					return f
				}
			}
		case []interface{}:
			for _, e := range t {
				if f := walk(e, inVolt); f > 0 {
					return f
				}
			}
		}
		return 0
	}
	return walk(v, false)
}

// fixedVoltage: the test profile's requested voltage (Braiins' minimum) and where it came from.
func fixedVoltage() (float64, string) {
	if raw, err := bos("GET", "/configuration/constraints", nil); err == nil {
		if v := parseMinVolt(raw); v > 0 {
			return v, "bosminer constraints"
		}
	}
	return fixedVoltFallback, "fallback (no voltage minimum in bosminer constraints)"
}

// manualModeBody: the SetPerformanceMode request for manual mode (bos-plus-api PerformanceService:
// mode = manual_mode {global_frequency {hertz}, global_voltage {volt}}).
// (verified on the miner, see manualModeBody): shape taken from the public proto + the gateway's snake_case JSON
// (GET /performance/mode answers {"tunermode":{...}}); the PUT bodies below are verified on BOS+ 26.11.
func manualModeBody(mhz, volt float64) map[string]interface{} {
	// Verified on BOS+ 26.11 (2026-11-25): PUT /performance/mode takes the oneof key without underscores at the top
	// ("manualmode" / "tunermode", as GET returns it), inner fields keep snake_case, and "hashboards" is required
	// (empty = all boards use the global values). The switch restarts bosminer; chains come up at the given values.
	return map[string]interface{}{"manualmode": map[string]interface{}{
		"global_frequency": map[string]float64{"hertz": mhz * 1e6},
		"global_voltage":   map[string]float64{"volt": volt},
		"hashboards":       []interface{}{},
	}}
}

// tunerModeBody: PUT /performance/mode body for tuner mode at power target watts (verified on BOS+ 26.11; same shape
// as GET /performance/mode returns).
func tunerModeBody(watts int) map[string]interface{} {
	return map[string]interface{}{"tunermode": map[string]interface{}{
		"target": map[string]interface{}{"powertarget": map[string]interface{}{
			"power_target": map[string]int{"watt": watts}}},
	}}
}

// modeIsManual: GET /performance/mode says manual mode (any spelling of the oneof key: manualmode, manual_mode,
// manualMode).
func modeIsManual(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	for k := range m {
		if strings.ReplaceAll(strings.ToLower(k), "_", "") == "manualmode" {
			return true
		}
	}
	return false
}

// manualRestoreW: the target restoreTunerMode must put back (0 = the miner is not in our manual mode). Persisted
// in wizard.json so a restart of s19volt still restores it.
func manualRestoreW() int { wizMu.Lock(); defer wizMu.Unlock(); return wiz.ManualRestore }

// setManualMode switches Braiins to manual mode (tuner off) at mhz on all chips and volt. The restore target is
// recorded BEFORE the request (a failed reply may still have switched the miner), then the mode is read back.
func setManualMode(mhz, volt float64, restoreW int) error {
	wizSet(func(s *wizState) { s.ManualRestore = restoreW })
	if raw, err := bos("GET", perfModePath, nil); err == nil && modeIsManual(raw) &&
		math.Abs(num(raw, "manualmode", "global_frequency", "hertz")/1e6-mhz) < 1 {
		note("fixed profile: already in manual mode at %.0f MHz - no switch (no bosminer restart)", mhz)
		return nil
	}
	if _, err := bos("PUT", perfModePath, manualModeBody(mhz, volt)); err != nil {
		return fmt.Errorf("manual mode refused: %w", err)
	}
	var raw json.RawMessage
	ok := waitFor(60*time.Second, 5*time.Second, func() bool {
		var err error
		raw, err = bos("GET", perfModePath, nil)
		return err == nil && modeIsManual(raw)
	})
	if !ok {
		return fmt.Errorf("manual mode not active after the request (mode: %s)", strings.TrimSpace(string(raw)))
	}
	note("fixed profile: manual mode %.0f MHz, %.2f V (tuner off)", mhz, volt)
	return nil
}

// restoreTunerMode puts Braiins back in tuner mode at power target watts and checks the configured target.
// The restore flag is cleared only on success.
func restoreTunerMode(watts int) error {
	if watts <= 0 {
		watts, _ = fixedProfile()
	}
	if _, err := bos("PUT", perfModePath, tunerModeBody(watts)); err != nil {
		return fmt.Errorf("tuner mode refused: %w", err)
	}
	if !waitFor(60*time.Second, 5*time.Second, func() bool {
		raw, err := bos("GET", perfModePath, nil)
		return err == nil && !modeIsManual(raw) && parseModeTarget(raw) == watts
	}) {
		return fmt.Errorf("tuner mode at %d W not confirmed", watts)
	}
	wizSet(func(s *wizState) { s.ManualRestore = 0 })
	note("tuner mode back at %d W", watts)
	return nil
}

// leaveManual: every wizard exit and SIGTERM. No-op unless our manual mode may be on; a failed restore alerts
// (the miner keeps the fixed profile: stock-region voltage, mid clocks - safe, but not what the owner set).
func leaveManual() {
	w := manualRestoreW()
	if w <= 0 {
		return
	}
	if err := restoreTunerMode(w); err != nil {
		note("setup exit: %v - the miner may still be in manual mode", err)
		go alert("manual_mode_left", fmt.Sprintf("could not put Braiins back in tuner mode at %d W (%v): set the power target in the Braiins UI", w, err))
	}
}

// restoreAfterRestart: s19volt stopped during the fixed profile -> put the tuner back once bosminer answers.
func restoreAfterRestart() {
	if manualRestoreW() <= 0 {
		return
	}
	waitFor(4*time.Minute, 10*time.Second, func() bool { return read().ok })
	// a resume within restoreGrace re-enters the test profile anyway: switching to the tuner and back would cost
	// two bosminer restarts for nothing. The wizard owns the mode while it runs.
	for t0 := time.Now(); time.Since(t0) < restoreGrace; time.Sleep(5 * time.Second) {
		if wizRunning() {
			return
		}
	}
	if !wizRunning() {
		leaveManual()
	}
}

// restoreGrace: how long s19volt waits after a restart in the test profile before putting the tuner back.
const restoreGrace = 3 * time.Minute

// fixedSettled: after the switch to the fixed profile, the chips run at (nearly) the profile clock and bosminer has
// not moved the PSU voltage for 45 s.
func fixedSettled(mhz float64) bool {
	m, err := fetchMetrics()
	if err != nil {
		return false
	}
	var fs []float64
	for _, b := range chipFreqMHz(m) {
		for _, f := range b {
			if f > 0 {
				fs = append(fs, f)
			}
		}
	}
	stMu.Lock()
	quiet := time.Since(tune.lastSet) >= 45*time.Second
	stMu.Unlock()
	return clocksReached(fs, mhz) && quiet
}

// clocksReached: the median chip clock is within 3 % of the profile clock.
func clocksReached(fs []float64, mhz float64) bool {
	if len(fs) == 0 || mhz <= 0 {
		return false
	}
	s := append([]float64(nil), fs...)
	sort.Float64s(s)
	med := s[len(s)/2]
	return med >= 0.97*mhz && med <= 1.03*mhz // both ways: chips still on the old (faster) tune are not "reached"
}

// Steady-rate detection (no fixed timer): board rates are read every steadyEvery; the miner is steady once the last
// steadyN readings of every enabled board agree within steadyTol of their mean.
const (
	steadyEvery = 30 * time.Second
	steadyN     = 3
	steadyTol   = 0.03
)

// waitSteadyRates: blocks until the board rates are steady, a stop is requested, or max elapses.
func waitSteadyRates(max time.Duration) {
	var hist []map[string]float64
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) && !stopRequested() {
		if r := read(); r.ok && r.mining && len(r.rates) > 0 {
			cp := map[string]float64{}
			for k, v := range r.rates {
				cp[k] = v
			}
			hist = append(hist, cp)
			if steady(hist, steadyN, steadyTol) {
				return
			}
		}
		wizSleep(steadyEvery)
	}
}

// steady: the last n readings of every board are within tol (fraction) of their mean.
func steady(hist []map[string]float64, n int, tol float64) bool {
	if len(hist) < n {
		return false
	}
	last := hist[len(hist)-n:]
	for b := range last[len(last)-1] {
		sum := 0.0
		for _, h := range last {
			v, ok := h[b]
			if !ok || v <= 0 {
				return false
			}
			sum += v
		}
		mean := sum / float64(n)
		for _, h := range last {
			if d := h[b]/mean - 1; d > tol || d < -tol {
				return false
			}
		}
	}
	return true
}
