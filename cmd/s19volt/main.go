// s19volt — the S19 undervolt service. Runs ON the miner's control board (Braiins OS+), next to bosminer:
// PSU voltage trim (V2: optocoupler driven from the empty 4th hashboard connector), burst-free level changes,
// fan rule, chip-temperature cap, calibration sweep and a web UI. A thermostat (e.g. Home Assistant) only picks levels.
//
//	GET /status            {"code": N, "volts": V, "events": [...]}   (reads the PSU setpoint, 0x03)
//	GET /boost?code=N      one bosminer-exact set frame (0x83), N in 5..255; returns when the PSU echoed it.
//	                       JSON has frame_sent_ms / done_ms so HA can time the relay click after it.
//	GET /unboost           same as /boost?code=255 (bosminer's 11.96 V)
//	GET /fan[?pct=N]       read / set bosminer's live manual fan target (via /etc/s19volt/s19regs), for HA's fan rules
//	GET /volts             {"boards": {"1": V, ...}, "mean": V, "min": V} board stack voltage from bosminer's own
//	                       metrics on 127.0.0.1:8081 (refreshes ~10 s), for HA's closed-loop optocoupler trim
//	GET /  + /api/*      miner-side web UI (web.go): status, 24 h history, controls (2026-11-24)
//	GET /state             {"last_set_age_s", "last_round_age_s", "last_round_target_w", "paused"} from bosminer's
//	                       log, so HA can wait until a profile has settled before it boosts
//
// Watchdog: follows /var/log/bosminer/bosminer.log; any line where bosminer itself touches the voltage or the
// tuning state (Setting voltage, start, HALT, PAUSE/RESUME, stage change, retune, Disable voltage) is POSTed to the
// optional -webhook URL (e.g. a Home Assistant webhook). Our own PSU writes never appear in bosminer's log.
// Open on the LAN like bosminer's own interfaces.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"s19psu/apw12"
)

var (
	port      = flag.Int("port", 8090, "HTTP port")
	webhook   = flag.String("webhook", "", "webhook for watchdog events (e.g. a Home Assistant webhook URL); empty = off")
	logPath   = flag.String("log", "/var/log/bosminer/bosminer.log", "bosminer log to follow")
	alertHook = flag.String("alert", "", "HA webhook for health alerts (boot, bad log signatures, pool/wallet); empty = off")
	noBus     = flag.Bool("nobus", false, "do not open the PSU I2C bus (UI development off the miner)")
	coreCfg   = flag.String("corecfg", "/etc/s19volt/core.json", "miner core config: loop modes + level table")
	webCfg    = flag.String("webcfg", "/etc/s19volt/web.json", "web UI config (bosminer login, UI password)")
	wallet    = flag.String("wallet", "", "pool user must contain this payout wallet/account (pool-hijack check); empty = off")

	psuMu    sync.Mutex // one PSU transaction at a time
	lastMu   sync.Mutex
	lastCode = 255 // what WE last set; bosminer resets the PSU itself on every start-up
	bus      *apw12.Bus
	evMu     sync.Mutex
	events   []map[string]string
	start    = time.Now()

	stMu   sync.Mutex // guards paused and the tune tracker (guard.go)
	paused bool

	sigRe   = regexp.MustCompile(`command: ff, status: ff|ERR:V4|Too many sensor errors|[Rr]eplace hashboard|panic`)
	watchRe = regexp.MustCompile(`PSU: Setting voltage|Starting BOSminer|HALT|--- PAUSE|--- RESUME|Entering STAGE|profile failed|retun|Disable voltage|PSU: Disable`)
)

func addEvent(kind, line string) {
	evMu.Lock()
	defer evMu.Unlock()
	events = append(events, map[string]string{"t": time.Now().UTC().Format(time.RFC3339), "kind": kind, "line": line})
	if len(events) > 50 {
		events = events[len(events)-50:]
	}
}

func jsonOut(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func setCode(n int) map[string]interface{} {
	if bus == nil {
		return map[string]interface{}{"ok": false, "error": "no PSU bus (-nobus)"}
	}
	psuMu.Lock()
	defer psuMu.Unlock()
	t0 := time.Now()
	res := map[string]interface{}{"code": n}
	bus.Mark = func(m string) {
		if m == "FRAME_SENT" {
			res["frame_sent_ms"] = time.Since(t0).Milliseconds()
		}
	}
	r, err := bus.BosCmd(0x83, []byte{byte(n), 0})
	bus.Mark = nil
	res["done_ms"] = time.Since(t0).Milliseconds()
	res["ok"] = err == nil && len(r) > 4 && int(r[4]) == n
	if err != nil {
		res["error"] = err.Error()
	}
	res["volts"] = apw12.BosCodeToV(n)
	if res["ok"] == true {
		lastMu.Lock()
		lastCode = n
		lastMu.Unlock()
	}
	addEvent("set", fmt.Sprintf("code %d ok=%v", n, res["ok"]))
	return res
}

func handleBoost(w http.ResponseWriter, r *http.Request) {
	if refuseDuringSetup(w) {
		return
	}
	n, err := strconv.Atoi(r.URL.Query().Get("code"))
	if r.URL.Path == "/unboost" {
		n, err = 255, nil
		lastMu.Lock()
		lc := lastCode
		lastMu.Unlock()
		if lc == 255 && r.URL.Query().Get("force") == "" { // never boosted (or bosminer reset it): stay off the bus
			jsonOut(w, map[string]interface{}{"ok": true, "code": 255, "skipped": "not boosted"})
			return
		}
	}
	if err != nil || n < 5 || n > 255 {
		w.WriteHeader(400)
		jsonOut(w, map[string]interface{}{"ok": false, "error": "code must be 5..255"})
		return
	}
	jsonOut(w, setCode(n))
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if bus == nil {
		jsonOut(w, map[string]interface{}{"error": "no PSU bus (-nobus)"})
		return
	}
	psuMu.Lock()
	rep, err := bus.BosCmd(0x03, nil)
	psuMu.Unlock()
	out := map[string]interface{}{"uptime_s": int(time.Since(start).Seconds())}
	if err == nil {
		out["code"] = int(rep[4])
		out["volts"] = apw12.BosCodeToV(int(rep[4]))
	} else {
		out["error"] = err.Error()
	}
	evMu.Lock()
	out["events"] = events
	evMu.Unlock()
	jsonOut(w, out)
}

func handleVolts(w http.ResponseWriter, r *http.Request) {
	body, err := fetchMetrics()
	if err != nil {
		w.WriteHeader(503)
		jsonOut(w, map[string]interface{}{"error": err.Error()})
		return
	}
	boards := boardVolts(body)
	if len(boards) == 0 {
		w.WriteHeader(503)
		jsonOut(w, map[string]interface{}{"error": "no hashboard_voltage in metrics"})
		return
	}
	sum, mn := 0.0, 99.0
	for _, v := range boards {
		sum += v
		if v < mn {
			mn = v
		}
	}
	jsonOut(w, map[string]interface{}{"boards": boards, "mean": sum / float64(len(boards)), "min": mn})
}

var fanRe = regexp.MustCompile(`fan target @0x[0-9a-f]+ = (\d+) %`)

// GET /fan            -> {"pct": N, "raw": "..."}   live manual fan target from bosminer's memory (s19regs fanget)
// GET /fan?pct=N      -> sets it (15..100) live, no restart; bosminer's own hot/dangerous overrides stay in charge
func handleFan(w http.ResponseWriter, r *http.Request) {
	args := []string{"fanget"}
	if q := r.URL.Query().Get("pct"); q != "" {
		if refuseDuringSetup(w) { // the setup owns the fans
			return
		}
		n, err := strconv.Atoi(q)
		if err != nil || n < 15 || n > 100 {
			w.WriteHeader(400)
			jsonOut(w, map[string]interface{}{"ok": false, "error": "pct must be 15..100"})
			return
		}
		args = []string{"fanset", strconv.Itoa(n)}
	}
	out, err := exec.Command("/etc/s19volt/s19regs", args...).CombinedOutput()
	res := map[string]interface{}{"raw": strings.TrimSpace(string(out))}
	if m := fanRe.FindStringSubmatch(string(out)); m != nil {
		n, _ := strconv.Atoi(m[1])
		res["pct"] = n
		res["ok"] = err == nil
	} else {
		res["ok"] = false
		w.WriteHeader(503)
	}
	jsonOut(w, res)
}

// alert posts a health event to HA's alert webhook (HA turns it into a push).
func alert(kind, detail string) {
	body, _ := json.Marshal(map[string]string{"event": kind, "detail": detail})
	c := &http.Client{Timeout: 5 * time.Second}
	for i := 0; i < 5; i++ {
		resp, err := c.Post(*alertHook, "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(time.Duration(5*(i+1)) * time.Second)
	}
	log.Printf("alert webhook failed for %s", kind)
}

// cgminer-compatible API on 127.0.0.1:4028 (bosminer): {"command":"pools"}
func cgPools() ([]map[string]interface{}, error) {
	raw, err := cgCommand("pools") // devs.go
	if err != nil {
		return nil, err
	}
	var r struct {
		POOLS []map[string]interface{}
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return r.POOLS, nil
}

// poolWatch: every 60 s; "no live pool" only while hashing is expected (not paused); wrong wallet always.
// 3 bad polls in a row -> alert; repeated every 6 h while it lasts; "cleared" when it ends.
func poolWatch() {
	streak := map[string]int{}
	active := map[string]time.Time{}
	sent := map[string]time.Time{}
	check := func(key string, bad bool, msg string) {
		if bad {
			streak[key]++
			if streak[key] >= 3 && active[key].IsZero() {
				active[key], sent[key] = time.Now(), time.Now()
				alert(key, msg)
			} else if !active[key].IsZero() && time.Since(sent[key]) > 6*time.Hour {
				sent[key] = time.Now()
				alert(key, "STILL: "+msg)
			}
			return
		}
		streak[key] = 0
		if !active[key].IsZero() {
			alert(key+"_cleared", fmt.Sprintf("%s cleared after %s", key, time.Since(active[key]).Round(time.Minute)))
			active[key] = time.Time{}
		}
	}
	for {
		time.Sleep(60 * time.Second)
		stMu.Lock()
		p := paused
		stMu.Unlock()
		pools, err := cgPools()
		if err != nil {
			continue // bosminer restarting: HA's own unreachable rule covers a dead miner
		}
		alive, bad := 0, []string{}
		for _, x := range pools {
			if s, _ := x["Status"].(string); s == "Alive" {
				alive++
			}
			if u, _ := x["User"].(string); *wallet != "" && u != "" && !strings.Contains(u, *wallet) {
				bad = append(bad, u)
			}
		}
		check("no_pool", !p && alive < 1, "no live pool")
		check("wallet", len(bad) > 0, "pool paying a wallet that isn't yours: "+strings.Join(bad, ","))
	}
}

func handleState(w http.ResponseWriter, r *http.Request) {
	stMu.Lock()
	defer stMu.Unlock()
	age := func(t time.Time) int {
		if t.IsZero() {
			return -1
		}
		return int(time.Since(t).Seconds())
	}
	jsonOut(w, map[string]interface{}{"last_set_age_s": age(tune.lastSet), "last_round_age_s": age(tune.lastRound),
		"last_round_target_w": tune.roundTarget, "last_settled_age_s": age(tune.lastSettled), "paused": paused})
}

func notify(kind, line string) {
	if *webhook == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"event": kind, "line": line})
	c := &http.Client{Timeout: 5 * time.Second}
	for i := 0; i < 3; i++ {
		resp, err := c.Post(*webhook, "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(time.Second)
	}
	log.Printf("webhook failed for %s", kind)
}

// follow the bosminer log from its current end; reopen on rotation/truncation.
func watch() {
	var f *os.File
	var rd *bufio.Reader
	var ino uint64
	var pos int64
	open := func(seekEnd bool) {
		if f != nil {
			f.Close()
		}
		var err error
		f, err = os.Open(*logPath)
		if err != nil {
			f = nil
			return
		}
		st, _ := f.Stat()
		ino = st.Sys().(*syscall.Stat_t).Ino
		if seekEnd {
			pos, _ = f.Seek(0, io.SeekEnd)
		} else {
			pos = 0
		}
		rd = bufio.NewReader(f)
	}
	open(false) // replay the current log once to learn the state, without alarms for old lines
	replaying := true
	last := map[string]time.Time{}
	for {
		if f == nil {
			time.Sleep(time.Second)
			open(false)
			continue
		}
		line, err := rd.ReadString('\n')
		if err == nil {
			pos += int64(len(line))
			ts := time.Now()
			if replaying && len(line) > 26 {
				if t, e := time.Parse("2006-01-02T15:04:05.000000Z", line[:27]); e == nil {
					ts = t
				}
			}
			stMu.Lock()
			tev := noteLine(line, ts) // guard.go: tuner state, stage-4 rounds, "Setting voltage", retunes
			if regexp.MustCompile(`--- PAUSE`).MatchString(line) {
				paused = true
			}
			if regexp.MustCompile(`--- RESUME|Starting BOSminer`).MatchString(line) {
				paused = false
			}
			if !replaying && regexp.MustCompile(`PSU: Setting voltage|Starting BOSminer`).MatchString(line) {
				lastMu.Lock()
				lastCode = 255 // bosminer wrote the PSU itself: our boost is gone
				lastMu.Unlock()
			}
			stMu.Unlock()
			if tev != "" && !replaying {
				wizEvent("braiins", "%s", tev) // the setup monitor's event list (only while the wizard runs)
			}
			if m := watchRe.FindString(line); m != "" && !replaying {
				if time.Since(last[m]) > 2*time.Second { // one webhook per kind per 2 s
					last[m] = time.Now()
					addEvent("bosminer", line)
					go notify(m, line)
				}
			}
			continue
		}
		if replaying {
			replaying = false
			log.Printf("replayed log up to byte %d", pos)
		}
		time.Sleep(200 * time.Millisecond)
		if st, e := os.Stat(*logPath); e == nil {
			if st.Sys().(*syscall.Stat_t).Ino != ino || st.Size() < pos {
				open(false) // rotated or truncated: read the new file from the start
			}
		}
	}
}

func main() {
	flag.Parse()
	if !*noBus {
		bus = apw12.New()
	}
	go watch()
	if *alertHook != "" {
		up := ""
		if b, e := os.ReadFile("/proc/uptime"); e == nil {
			up = strings.Fields(string(b))[0]
		}
		go alert("boot", "s19volt started, miner uptime "+up+" s")
		go poolWatch()
	}
	http.HandleFunc("/status", handleStatus)
	http.HandleFunc("/boost", handleBoost)
	http.HandleFunc("/unboost", handleBoost)
	http.HandleFunc("/state", handleState)
	http.HandleFunc("/volts", handleVolts)
	http.HandleFunc("/fan", handleFan)
	setupWeb(*webCfg) // GET / = web UI, /api/* (2026-11-24)
	setupTrimDriver() // 4th-connector GPIO PWM; before the core uses it
	go func() {       // on stop/kill: V2 pin back LOW (stock voltage) before exiting
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
		<-sig
		closeTrimDriver()  // glides toward 0 % for at most a minute, then forces the pin LOW
		if !wizRunning() { // a run in progress keeps its test profile: the next start resumes it or, if no resume
			leaveManual() // comes within restoreGrace, puts the tuner back (no tuner<->manual double switch on a deploy)
		}
		os.Exit(0)
	}()
	setupCore(*coreCfg) // power changes, trim loop, fan rule, chip-temperature cap (2026-11-24)
	log.Printf("s19volt on :%d, webhook %s", *port, *webhook)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}
