// web.go — miner-side web interface for the S19 heater (usable without Home Assistant).
//
//	GET  /                 single-page UI (embedded ui/index.html)
//	GET  /api/status       everything the page shows: bosminer REST (stats, hashboards, cooling, tuner), board volts,
//	                       live fan target, PSU code, trim, s19volt state + events. Cached 4 s.
//	GET  /api/history      30-s samples, last 24 h (in memory)
//	POST /api/control      {"action": "power"|"boards"|"fan"|"trim"|"pause"|"resume", ...}; header X-Auth = ui_password (only if one is set)
//
// Config: /etc/s19volt/web.json (mode 600) — bosminer REST login, UI password, power levels offered.
// Who controls the level is not configured: s19volt tracks the last external /api/level or /api/control call
// (controllerState) and the page shows "Setup running", "API (last request N min ago, from <address>)" or "Manual".
package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui
var uiFS embed.FS

type webConfig struct {
	BosUser    string `json:"bos_user"` // default root
	BosPass    string `json:"bos_pass"`
	UIPassword string `json:"ui_password"` // optional: empty = controls open on the LAN (default); set = X-Auth required
	Levels     []int  `json:"levels"`      // power targets offered in the UI
}

var (
	wcfg  webConfig
	httpc = &http.Client{Timeout: 10 * time.Second}

	stCacheMu sync.Mutex
	stCache   map[string]interface{}
	stCacheAt time.Time

	histMu sync.Mutex
	hist   []map[string]interface{}
)

func loadWebConfig(path string) {
	wcfg = webConfig{BosUser: "root", Levels: []int{360, 380, 450, 520, 700, 800, 900, 1000, 1100, 1200, 1300,
		1400, 1500, 1600, 1700, 1800, 1900, 2000, 2100, 2200}}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("web: no config %s (%v) — status only, controls disabled", path, err)
		return
	}
	if err = json.Unmarshal(b, &wcfg); err != nil {
		log.Printf("web: bad config %s: %v", path, err)
	}
}

// ---- who controls the level: the last external API call ----

var (
	apiMu   sync.Mutex
	apiAddr string
	apiAt   time.Time
)

// apiRecent: an external call within this window means "the API is in control".
const apiRecent = 60 * time.Minute

// pageHeader marks calls made by the heater page itself (manual use, not an external controller).
const pageHeader = "X-S19-Page"

// noteAPICall records the remote address and time of an external /api/level or /api/control call.
func noteAPICall(r *http.Request) {
	if r.Header.Get(pageHeader) != "" {
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	apiMu.Lock()
	apiAddr, apiAt = host, time.Now()
	apiMu.Unlock()
}

// controllerState: "setup" while a setup run (who: wizard | calibrate) owns the miner; "api" when an external call came within apiRecent
// (age in whole minutes); else "manual".
func controllerState(who, addr string, at, now time.Time) map[string]interface{} {
	switch {
	case who != "":
		return map[string]interface{}{"kind": "setup", "who": who}
	case !at.IsZero() && now.Sub(at) <= apiRecent:
		return map[string]interface{}{"kind": "api", "addr": addr, "age_min": int(now.Sub(at).Minutes())}
	}
	return map[string]interface{}{"kind": "manual"}
}

func controllerNow() map[string]interface{} {
	apiMu.Lock()
	a, t := apiAddr, apiAt
	apiMu.Unlock()
	coreMu.Lock()
	who := setupWho
	coreMu.Unlock()
	return controllerState(who, a, t, time.Now())
}

// ---- local readings shared with the existing endpoints ----

func voltsMap() map[string]interface{} {
	body, err := fetchMetrics()
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	return map[string]interface{}{"boards": boardVolts(body)}
}

// fanSet sets bosminer's live manual fan target (via s19regs). The one place that writes the fan.
func fanSet(pct int) (string, error) {
	out, err := exec.Command("/etc/s19volt/s19regs", "fanset", strconv.Itoa(pct)).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func fanPct() interface{} {
	out, _ := exec.Command("/etc/s19volt/s19regs", "fanget").CombinedOutput()
	if m := fanRe.FindStringSubmatch(string(out)); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return nil
}

func collectStatus() map[string]interface{} {
	out := map[string]interface{}{"t": time.Now().Unix(), "uptime_s": int(time.Since(start).Seconds()),
		"controller": controllerNow(), "levels": wcfg.Levels, "controls": true, "auth": wcfg.UIPassword != ""}
	var wg sync.WaitGroup
	var mu sync.Mutex
	set := func(k string, v interface{}) { mu.Lock(); out[k] = v; mu.Unlock() }
	for _, p := range []struct{ key, path string }{{"stats", "/miner/stats"}, {"hashboards", "/miner/hw/hashboards"},
		{"cooling", "/cooling/state"}, {"tuner", "/performance/tuner-state"}} {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			if raw, err := bos("GET", p.path, nil); err == nil {
				set(p.key, raw)
			} else {
				set(p.key+"_error", err.Error())
			}
		}()
	}
	wg.Add(4)
	go func() {
		defer wg.Done()
		if r := read(); r.ok && r.mining {
			t := r.target
			if t <= 0 && manualRestoreW() > 0 { // the wizard's fixed test profile (manual mode: no tuner target)
				if f := wizSnap().Fixed; f != nil && f.Target > 0 {
					t = f.Target
					set("power_manual_mhz", f.MHz)
				}
			}
			if t > 0 {
				set("power_target", t)
				set("est_wall_w", math.Round(estWall(t, r.volts))) // fitted model, see core.go
			}
		}
	}()
	go func() { defer wg.Done(); set("volts", voltsMap()) }()
	go func() { defer wg.Done(); set("fan_pct", fanPct()) }()
	go func() {
		defer wg.Done()
		if drv != nil {
			t, _ := drv.Target()
			a, _ := drv.Applied()
			set("trim", map[string]interface{}{"target": t, "applied": a, "driver": drv.Name()})
		}
	}()
	wg.Wait()
	stMu.Lock()
	out["state"] = map[string]interface{}{"paused": paused, "last_round_target_w": tune.roundTarget}
	out["braiins_tuner"] = tune.view(time.Now()) // parsed from the bosminer log (guard.go); "tuner" is bosminer's REST
	stMu.Unlock()
	evMu.Lock()
	ev := make([]map[string]string, len(events))
	copy(ev, events)
	evMu.Unlock()
	out["events"] = ev
	return out
}

func cachedStatus() map[string]interface{} {
	stCacheMu.Lock()
	defer stCacheMu.Unlock()
	if stCache == nil || time.Since(stCacheAt) > 4*time.Second {
		stCache, stCacheAt = collectStatus(), time.Now()
	}
	return stCache
}

// ---- history: one compact sample every 30 s, 24 h ----

func num(raw interface{}, path ...string) float64 {
	var v interface{}
	switch r := raw.(type) {
	case json.RawMessage:
		_ = json.Unmarshal(r, &v)
	default:
		v = r
	}
	for _, k := range path {
		m, ok := v.(map[string]interface{})
		if !ok {
			return 0
		}
		v = m[k]
	}
	f, _ := v.(float64)
	return f
}

func sampler() {
	for {
		s := cachedStatus()
		ths := num(s["stats"], "miner_stats", "real_hashrate", "last_1m", "gigahash_per_second") / 1000
		smp := map[string]interface{}{"t": s["t"], "ths": ths,
			"target_w": num(s["tuner"], "mode_state", "powertargetmodestate", "current_target", "watt"),
			"chip_c":   num(s["cooling"], "highest_temperature", "temperature", "degree_c"), "paused": false}
		if v, ok := s["volts"].(map[string]interface{}); ok {
			if b, ok := v["boards"].(map[string]float64); ok && len(b) > 0 {
				sum, n := 0.0, 0
				for _, x := range b {
					if x > 1 { // a disabled board reads 0 V
						sum += x
						n++
					}
				}
				if n > 0 {
					smp["volts"] = sum / float64(n)
				}
			}
		}
		if t, ok := s["trim"].(map[string]interface{}); ok {
			smp["trim"] = t["applied"]
		}
		if st, ok := s["state"].(map[string]interface{}); ok {
			smp["paused"] = st["paused"]
		}
		histMu.Lock()
		hist = append(hist, smp)
		if len(hist) > 2880 {
			hist = hist[len(hist)-2880:]
		}
		histMu.Unlock()
		time.Sleep(30 * time.Second)
	}
}

// ---- control ----

func handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if !authOK(r) {
		w.WriteHeader(401)
		jsonOut(w, map[string]interface{}{"ok": false, "error": "wrong or missing password"})
		return
	}
	noteAPICall(r)
	if refuseDuringSetup(w) { // every action: the setup owns trim, power, fans and boards
		return
	}
	var req struct {
		Action string   `json:"action"`
		Watts  int      `json:"watts"`
		Boards []string `json:"boards"` // boards to keep ON, e.g. ["1","3"]
		Pct    float64  `json:"pct"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		jsonOut(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	var err error
	var res interface{}
	switch req.Action {
	case "power":
		if req.Watts < 360 || req.Watts > 2200 {
			err = errors.New("watts must be 360..2200")
			break
		}
		res, err = bos("PUT", "/performance/power-target", map[string]int{"watt": req.Watts})
	case "boards":
		on, off := []string{}, []string{}
		keep := map[string]bool{}
		for _, b := range req.Boards {
			if b == "1" || b == "2" || b == "3" {
				keep[b] = true
			}
		}
		if len(keep) == 0 {
			err = errors.New("keep at least one board on")
			break
		}
		for _, b := range []string{"1", "2", "3"} {
			if keep[b] {
				on = append(on, b)
			} else {
				off = append(off, b)
			}
		}
		if res, err = bos("PATCH", "/miner/hw/hashboards", map[string]interface{}{"enable": true, "hashboard_ids": on}); err == nil && len(off) > 0 {
			time.Sleep(5 * time.Second)
			res, err = bos("PATCH", "/miner/hw/hashboards", map[string]interface{}{"enable": false, "hashboard_ids": off})
		}
	case "fan":
		if req.Pct < 15 || req.Pct > 100 {
			err = errors.New("fan must be 15..100 %")
			break
		}
		res, err = fanSet(int(req.Pct))
	case "trim":
		if req.Pct < 0 || req.Pct > ccfg.TrimCap { // hardware floor (33k series resistor) is the real limit
			err = fmt.Errorf("trim must be 0..%.0f %%", ccfg.TrimCap)
			break
		}
		if drv == nil {
			err = errors.New("no trim hardware configured")
			break
		}
		err = drv.Set(req.Pct)
		res = map[string]interface{}{"trim": req.Pct}
	case "pause":
		res, err = bos("PUT", "/actions/pause", nil)
	case "resume":
		res, err = bos("PUT", "/actions/resume", nil)
	default:
		err = fmt.Errorf("unknown action %q", req.Action)
	}
	addEvent("ui", fmt.Sprintf("%s %v %v %v -> err=%v", req.Action, req.Watts, req.Boards, req.Pct, err))
	stCacheMu.Lock()
	stCache = nil
	stCacheMu.Unlock()
	if err != nil {
		w.WriteHeader(502)
		jsonOut(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	jsonOut(w, map[string]interface{}{"ok": true, "result": res})
}

func setupWeb(cfgPath string) {
	loadWebConfig(cfgPath)
	sub, _ := fs.Sub(uiFS, "ui")
	http.Handle("/", http.FileServer(http.FS(sub)))
	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, cachedStatus()) })
	// live (uncached) trim state for Home Assistant's trim entity
	http.HandleFunc("/api/trim", func(w http.ResponseWriter, r *http.Request) {
		if drv == nil {
			jsonOut(w, map[string]interface{}{"available": false, "target": 0, "applied": 0})
			return
		}
		t, _ := drv.Target()
		a, _ := drv.Applied()
		out := map[string]interface{}{"available": true, "target": t, "applied": a, "cap": ccfg.TrimCap}
		duty, late, rt := drv.pwmStats()
		out["duty_measured"], out["worst_late_us"], out["pwm_priority"] = math.Round(duty*100)/100, late, rt
		jsonOut(w, out)
	})
	http.HandleFunc("/api/history", func(w http.ResponseWriter, r *http.Request) {
		histMu.Lock()
		h := make([]map[string]interface{}, len(hist))
		copy(h, hist)
		histMu.Unlock()
		jsonOut(w, h)
	})
	http.HandleFunc("/api/control", handleControl)
	go sampler()
}
