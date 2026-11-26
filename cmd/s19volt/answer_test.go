package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStepPass96(t *testing.T) {
	for _, c := range []struct {
		weak      int
		ths, chip float64
		want      bool
		why       string
	}{
		{0, 96.0, 70, true, "exactly 96 %"},
		{1, 99.0, 70, true, "one noise chip"},
		{2, 99.0, 70, false, "two newly weak chips"},
		{0, 95.9, 70, false, "below 96 % total"},
		{0, 99.0, 80.5, false, "chips above target + 6"},
	} {
		if got := stepPass(c.weak, c.ths, 100, c.chip, 74); got != c.want {
			t.Errorf("%s: stepPass = %v", c.why, got)
		}
	}
	if stepPass(0, 10, 0, 60, 74) {
		t.Error("no baseline must never pass")
	}
}

func TestAnswerStep(t *testing.T) {
	steps := []wizStep{
		{V: 10.0, THs: 100, JTH: 30, Pass: true},
		{V: 9.8, THs: 90, JTH: 29, Pass: false},
		{V: 9.9, THs: 99, JTH: 29, Pass: true},         // the answer (best passing J/TH)
		{V: 9.85, THs: 99, JTH: 28, Invalid: "retune"}, // thrown out by the profile guard
		{V: 9.7, Dropped: "board 2", Pass: true},       // hold aborted: never an answer
	}
	if i := answerStep(steps); i != 2 {
		t.Fatalf("answerStep = %d, want 2", i)
	}
	// best J/TH wins, not the most drive (run 7, 600 W)
	run7 := []wizStep{
		{V: 9.56, Drive: 62.8, THs: 25.0, JTH: 22.73, Pass: false},
		{V: 9.81, Drive: 61.5, THs: 26.53, JTH: 23.04, Pass: true},
		{V: 9.69, Drive: 62.2, THs: 25.92, JTH: 23.15, Pass: true},
		{V: 9.67, Drive: 62.5, THs: 25.81, JTH: 23.15, Pass: true},
		{V: 9.64, Drive: 62.7, THs: 25.48, JTH: 23.35, Pass: true},
	}
	if i := answerStep(run7); i != 1 {
		t.Fatalf("answerStep run 7 = %d, want 1 (9.81 V, best J/TH)", i)
	}
	tie := []wizStep{
		{V: 10.40, Drive: 55, THs: 100, JTH: 30, Pass: true},
		{V: 10.30, Drive: 57, THs: 100, JTH: 30, Pass: true},
	}
	if i := answerStep(tie); i != 0 {
		t.Fatalf("equal J/TH must pick the less drive, got %d", i)
	}
	if answerStep(nil) != -1 || answerStep(steps[1:2]) != -1 {
		t.Fatal("no passing step must give -1")
	}
}

func TestAnswerSafety(t *testing.T) {
	if !effSafe(true, nil) || effSafe(true, []string{"2/36"}) || effSafe(false, nil) {
		t.Error("the answer is safe only with per-chip data and nothing latched")
	}
}

func TestOldStateWithStrictLoads(t *testing.T) {
	old := `{"inputs":{"min_w":500,"answer":"strict","margin_v":0.05},"levels":[{"watts":500,"answer":"strict",
"strict":{"v":10.8,"safe":true},"eff":{"v":10.5,"safe":false},"steps":[{"v":10.8,"strict":true,"stable":true,"full":true}]}]}`
	var s wizState
	if err := json.Unmarshal([]byte(old), &s); err != nil {
		t.Fatalf("old state with strict fields: %v", err)
	}
	if s.Inputs.MinW != 500 || s.Inputs.MarginV != 0.05 || len(s.Levels) != 1 || s.Levels[0].Eff == nil || !s.Levels[0].Steps[0].Stable {
		t.Fatalf("old state not read: %+v", s)
	}
}

func TestNewWeakAndBoardDrop(t *testing.T) {
	got := newWeakList(map[string]bool{"2/36": true, "1/4": true, "3/9": true}, map[string]bool{"3/9": true})
	if len(got) != 2 || got[0] != "1/4" || got[1] != "2/36" {
		t.Fatalf("newWeakList = %v", got)
	}
	ref := map[string]float64{"1": 30, "2": 30, "3": 30}
	if b := boardDropped(map[string]float64{"1": 29, "2": 18.1, "3": 30}, ref, 0.6); b != "" {
		t.Errorf("60.3 %% is not a drop: %q", b)
	}
	if b := boardDropped(map[string]float64{"1": 29, "2": 17, "3": 30}, ref, 0.6); b != "2" {
		t.Errorf("board 2 at 57 %%: %q", b)
	}
	if b := boardDropped(map[string]float64{"1": 29, "2": 29}, ref, 0.6); b != "3" {
		t.Errorf("a board with no reading counts as dropped: %q", b)
	}
}

func TestClampTarget(t *testing.T) {
	if v, r := clampTarget(320, 360); v != 360 || !r {
		t.Errorf("320 -> %d %v", v, r)
	}
	if v, r := clampTarget(500, 360); v != 500 || r {
		t.Errorf("500 -> %d %v", v, r)
	}
	if v, r := clampTarget(300, 0); v != 300 || r {
		t.Errorf("no minimum known -> %d %v", v, r)
	}
	if w := parseMinTarget(json.RawMessage(`{"tuner_constraints":{"power_target":{"min":{"watt":359},"max":{"watt":4000}}}}`)); w != 359 {
		t.Errorf("parseMinTarget snake = %d", w)
	}
	if w := parseMinTarget(json.RawMessage(`{"tunerConstraints":{"powerTarget":{"min":{"watt":359}}}}`)); w != 359 {
		t.Errorf("parseMinTarget camel = %d", w)
	}
	if w := parseMinTarget(json.RawMessage(`{"other":1}`)); w != 0 {
		t.Errorf("parseMinTarget missing = %d", w)
	}
}

func TestPace(t *testing.T) {
	// 0 -> 50 % at 5 % per step: 10 steps, never more than 5 % at once, both directions
	cur, n := 0.0, 0
	for cur != 50 {
		nx := nextTrim(cur, 50, paceStep)
		if nx-cur > paceStep+1e-9 || nx < cur {
			t.Fatalf("step %v -> %v", cur, nx)
		}
		cur, n = nx, n+1
	}
	if n != 10 {
		t.Fatalf("%d steps up, want 10", n)
	}
	if nextTrim(52, 0, 5) != 47 || nextTrim(3, 0, 5) != 0 || nextTrim(48, 50, 5) != 50 {
		t.Fatal("nextTrim clamps at the target")
	}
	if paceStep/paceDelay.Seconds() != 0.5 {
		t.Fatal("setup pace must be 5 % per 10 s")
	}
}

// exact lines from the miner's log (Braiins OS+ 26.11 nightly, 2026-11-25)
const (
	lnRound   = "2026-11-25T10:00:00.000000Z INFO bosminer_plus_tuner::tune: Tune/all: S4 it3: target 2000.00 W, measured n/a, estimated 1935.62 W, 12.021 V | hb1 539.0 MHz mcr 100.0% | ..."
	lnTuned   = "2026-11-25T10:00:00.000000Z INFO bosminer_plus_tuner::tune::stages::stage4: Tune/all: Stage 4: Profile tuned at inlet 36.5°C / outlet 59.5°C, current inlet 34.5°C / outlet 58.5°C"
	lnRamp    = "2026-11-25T10:00:00.000000Z INFO bosminer_plus_tuner::tune::experiment: Tune/all: it4: ramping to 12.021 V, 538.6 MHz"
	lnMeasure = "2026-11-25T10:00:00.000000Z INFO bosminer_plus_tuner::tune::experiment: Tune/all: Status: measuring performance for 499 seconds"
	lnSet     = "2026-11-25T10:00:00.000000Z INFO bosminer_backend::psu: PSU: Setting voltage 12.137 V -> 12.097 V"
)

func TestClassifyLine(t *testing.T) {
	for _, c := range []struct {
		line string
		want lineKind
	}{
		{lnRound, lineRound},
		{lnTuned, lineOther},
		{lnRamp, lineOther},
		{lnMeasure, lineOther},
		{lnSet, lineSetVoltage},
		{"INFO bosminer_plus_tuner::tune: Entering STAGE 4: stable", lineOther},
		{"INFO bosminer_plus_tuner::tune: Entering STAGE 1: ...", lineRetune},
		{"INFO bosminer_plus_tuner::tune: Entering STAGE 3", lineRetune},
		{"WARN bosminer_plus_tuner: Profile failed, retuning", lineRetune},
		{"WARN tuner: profile failed", lineRetune},
		{"INFO Starting BOSminer 26.11", lineRetune},
		{"INFO pool: share accepted", lineOther},
	} {
		if k, _ := classifyLine(c.line); k != c.want {
			t.Errorf("classifyLine(%q) = %v, want %v", c.line, k, c.want)
		}
	}
	if _, w := classifyLine(lnRound); w != 2000 {
		t.Errorf("round target = %d", w)
	}
}

func TestTuneTrackerSettled(t *testing.T) {
	var k tuneTracker
	t0 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	at := func(m float64) time.Time { return t0.Add(time.Duration(m * float64(time.Minute))) }
	routine := func(m float64) { // the routine lines of one round: never a retune, never a voltage change
		for _, l := range []string{lnTuned, lnRamp, lnMeasure} {
			k.feed(l, at(m))
		}
	}
	k.feed(lnRound, at(0)) // first round after the change: no clean predecessor
	if k.settledSince(2000, t0.Add(-time.Second)) {
		t.Fatal("a first round is not settled")
	}
	routine(0)
	k.feed(lnSet, at(0.1)) // round it4 moves the voltage (stage-4 settling)
	k.feed(lnSet, at(0.15))
	k.feed(lnRound, at(8.5))
	if k.settledSince(2000, t0) {
		t.Fatal("a round with Setting voltage is not settled")
	}
	if set, re := k.since(t0); set == "" || re != "" {
		t.Fatalf("Setting voltage is a voltage move, not a retune: %q %q", set, re)
	}
	routine(8.5) // next round: ramping to the same voltage, no Setting voltage
	k.feed(lnRound, at(17))
	if !k.settledSince(2000, t0) {
		t.Fatal("a round without Setting voltage after the previous one is settled")
	}
	if k.settledSince(1900, t0) {
		t.Fatal("settled is per target")
	}
	if set, re := k.since(at(9)); set != "" || re != "" {
		t.Fatalf("routine stage-4 lines are no event: %q %q", set, re)
	}
	k.feed("INFO Entering STAGE 2", at(20)) // a real retune: the next round has no clean predecessor
	if _, re := k.since(at(18)); re == "" {
		t.Fatal("stage 2 is a retune")
	}
	k.feed(lnRound, at(30))
	if k.settledSince(2000, at(18)) {
		t.Fatal("the first round after a retune is not settled")
	}
}

func TestTouchVerdict(t *testing.T) {
	if touchVerdict(false, 0, 0) != "" || touchVerdict(false, 3, 0) != "" {
		t.Error("Setting voltage redos are allowed up to the cap")
	}
	if touchVerdict(false, maxRedos, 0) == "" {
		t.Error("the total cap fails the level")
	}
	if touchVerdict(true, 2, 0) != "" {
		t.Error("the first retune gets a redo")
	}
	if touchVerdict(true, 1, 1) == "" {
		t.Error("a second retune fails the level")
	}
}

func TestLevelMinEstimate(t *testing.T) {
	// first level of a board set: 4 clean + 20 Braiins + (5 baseline + 4 x (1 + 5) steps + 2 x 4 resets + 3 + 1 latch)
	if m := levelMin(true); m != 4+20+22+8+4 {
		t.Fatalf("levelMin(first) = %d", m)
	}
	if m := levelMin(false); m != 4+20+17+4+4 {
		t.Fatalf("levelMin(later) = %d", m)
	}
	in := wizInputs{MinW: 300, MaxW: 800, StepW: 100, FewerBoards: true}
	j := in.jobs()
	if f := firstJobs(j); f != 3 {
		t.Fatalf("firstJobs = %d (123, n2, n1)", f)
	}
	if m := searchMinutes(j); m != 3*levelMin(true)+(len(j)-3)*levelMin(false) {
		t.Fatalf("searchMinutes = %d", m)
	}
	if levelMaxMin() <= levelMin(true) {
		t.Fatal("worst case below typical")
	}
}

func TestFanRules(t *testing.T) {
	if f := preResumeFan(30, 18, 25); f != 30 {
		t.Errorf("never below the current fan: %d", f)
	}
	if f := preResumeFan(15, 18, 25); f != 25 {
		t.Errorf("power-matched: %d", f)
	}
	if f := preResumeFan(15, 22, 20); f != 22 {
		t.Errorf("base fan: %d", f)
	}
	if matchFan(500, 15, 30) != 15 || matchFan(2000, 15, 30) != 25 || matchFan(3000, 15, 30) != 30 {
		t.Error("matchFan")
	}
	if exitLevel(0, 1200) != 1200 || exitLevel(900, 1200) != 900 {
		t.Error("exitLevel")
	}
}

func TestStopFlag(t *testing.T) {
	stopFlag.Store(false)
	if !stoppableSleep(time.Millisecond) {
		t.Fatal("no stop: the sleep completes")
	}
	requestStop()
	t0 := time.Now()
	if stoppableSleep(time.Minute) || stoppableSleep(time.Minute) || !calStopped() {
		t.Fatal("a stop ends every later wait of the run (not one-shot)")
	}
	if time.Since(t0) > time.Second {
		t.Fatal("a stopped sleep returns at once")
	}
	coreMu.Lock()
	phase, setupWho = "idle", ""
	coreMu.Unlock()
	if ok, _ := setupBegin("wizard", "wizard"); !ok || stopRequested() {
		t.Fatal("setupBegin clears a stale stop")
	}
	setupEnd()
}

func TestClaimExclusive(t *testing.T) {
	coreMu.Lock()
	phase, setupWho = "idle", ""
	coreMu.Unlock()
	if !claimChange() {
		t.Fatal("idle core: the worker claims")
	}
	if ok, _ := setupBegin("wizard", "wizard"); ok {
		t.Fatal("a setup must not start during a level change")
	}
	setPhase("idle")
	if ok, _ := setupBegin("wizard", "wizard"); !ok {
		t.Fatal("setup on an idle core")
	}
	if claimChange() {
		t.Fatal("the worker must not claim during a setup")
	}
	setupEnd()
	coreMu.Lock()
	ph, who := phase, setupWho
	coreMu.Unlock()
	if ph != "idle" || who != "" {
		t.Fatalf("setupEnd: %q %q", ph, who)
	}
}

// profile guard snapshot on a synthetic autotune file: timestamps do not count, clocks do.
func TestProfileSnapshot(t *testing.T) {
	mk := func(secs, hz float64) []byte {
		chip := map[string]interface{}{"pll_frequencies": []interface{}{hz}}
		cfg := map[string]interface{}{"params": map[string]interface{}{"chip_params_set": map[string]interface{}{"inner": []interface{}{chip}}}}
		entry := func(w float64) map[string]interface{} {
			return map[string]interface{}{"profile_target": map[string]interface{}{"Power": w},
				"chains":     []interface{}{1, 2, 3},
				"created_on": map[string]interface{}{"secs_since_epoch": secs},
				"stage":      map[string]interface{}{"S4": map[string]interface{}{"multi_configuration": map[string]interface{}{"configurations": []interface{}{cfg}}}}}
		}
		b, _ := json.Marshal(map[string]interface{}{"version": 9, "boards": map[string]interface{}{"1000W-a": entry(1000), "1100W-b": entry(1100)}})
		return b
	}
	old, oldBk := autotuneFile, tuneBackupDir
	defer func() { autotuneFile, tuneBackupDir = old, oldBk }()
	autotuneFile, tuneBackupDir = filepath.Join(t.TempDir(), "autotune.json"), t.TempDir()
	write := func(b []byte) {
		if err := os.WriteFile(autotuneFile, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(mk(1, 500e6))
	a, err := profileSnapshot(1000, 3)
	if err != nil || a == "" {
		t.Fatalf("snapshot: %q %v", a, err)
	}
	write(mk(2, 500e6)) // re-saved, same tune
	if b, _ := profileSnapshot(1000, 3); b != a {
		t.Fatal("a timestamp-only change must not count")
	}
	write(mk(2, 490e6)) // retuned
	if b, _ := profileSnapshot(1000, 3); b == a {
		t.Fatal("a clock change must count")
	}
	if b, _ := profileSnapshot(1000, 2); b != "" {
		t.Fatal("other board count must not match")
	}
	orig, _ := profileSnapshot(1100, 3)
	saved, err := profileEntriesSaved(1100, 3)
	if err != nil || len(saved) != 1 {
		t.Fatalf("profileEntriesSaved: %v %v", saved, err)
	}
	if n, err := scaleProfileClocks(1100, 3, 0.9); err != nil || n != 1 {
		t.Fatalf("scaleProfileClocks on the shared entry finder: %d %v", n, err)
	}
	if b, _ := profileSnapshot(1100, 3); b == orig {
		t.Fatal("scaled clocks must differ")
	}
	other, _ := profileSnapshot(1000, 3)
	if err := restoreProfileEntries(saved); err != nil {
		t.Fatal(err)
	}
	if b, _ := profileSnapshot(1100, 3); b != orig {
		t.Fatal("restore must put the original clocks back")
	}
	if b, _ := profileSnapshot(1000, 3); b != other {
		t.Fatal("restore must keep the other entries")
	}
	if _, err := profileEntriesSaved(1234, 3); err == nil {
		t.Fatal("no entry: error")
	}
}

func TestParseModeTarget(t *testing.T) {
	if w := parseModeTarget(json.RawMessage(`{"tunermode":{"target":{"powertarget":{"power_target":{"watt":2200}}}}}`)); w != 2200 {
		t.Fatalf("got %d", w)
	}
	if w := parseModeTarget(json.RawMessage(`{"overall_tuner_state":1,"mode_state":null}`)); w != 0 {
		t.Fatalf("got %d", w)
	}
}
