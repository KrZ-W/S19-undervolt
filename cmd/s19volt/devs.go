package main

// devs.go — exact window hashrate from bosminer's cgminer-compatible API (stock Braiins OS+): 127.0.0.1:4028,
// {"command":"devs"} -> DEVS[] per board with "Total MH" (cumulative MH since bosminer started) and "Device Elapsed"
// (s). A window's per-board rate = the work done between the two reads / the wall time between them: the exact
// average over the window, not a mean of 1-min readings. One helper (exactRates) for the stock baseline, every hold
// and the latch windows. Port 4028 unavailable -> the board rates (API last_1m mean) as before, logged once.
//
// The work counter is "Diff1 Work" (one count = 2^40 hashes), not "Total MH": measured 2026-11-26 on bosminer 26.11,
// Total MH advances in lumps of ~25 s of a board's work (30-s deltas of 0 / 20 / 38 / 57 TH/s on a 24 TH/s board),
// so a 5-min window holds only ~12 lumps per board: +-30 % per board, +-10-15 % on the total (it read a 1600 W hold
// at 71 TH/s against a real 63). Diff1 Work advances ~1300 counts/min/board, evenly: ~+-1 % per board over 5 min.
// Total MH stays the fallback for a reply without Diff1 Work.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"
)

// cgCommand: one command to the cgminer-compatible API (bosminer answers and closes; trailing NUL trimmed).
func cgCommand(cmd string) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:4028", 8*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err = conn.Write([]byte(`{"command":"` + cmd + `"}`)); err != nil {
		return nil, err
	}
	raw, _ := io.ReadAll(conn)
	return bytes.TrimRight(raw, "\x00\n "), nil
}

// devEntry: one DEVS[] element (the fields the window math needs).
type devEntry struct {
	asc     int
	id      string
	enabled bool
	mh, el  float64 // Total MH, Device Elapsed (s)
	d1      float64 // Diff1 Work (counts of diff1Hashes); hasD1 = the reply carried it
	hasD1   bool
}

// diff1Hashes: hashes per "Diff1 Work" count (bosminer 26.11: cumulative Total MH / Diff1 Work = 2^39.9-2^40.1 per
// board after 80 min, the spread being Total MH's lumps).
const diff1Hashes = 1 << 40

// devsSnap: one devs reply and when it was read (ok = parsed).
type devsSnap struct {
	ok   bool
	at   time.Time
	devs []devEntry
}

// parseDevs: a devs reply -> its entries. Numbers may come as JSON numbers or strings.
func parseDevs(raw []byte, at time.Time) devsSnap {
	var r struct {
		DEVS []map[string]interface{}
	}
	if json.Unmarshal(raw, &r) != nil || len(r.DEVS) == 0 {
		return devsSnap{}
	}
	f := func(m map[string]interface{}, k string) (float64, bool) {
		switch v := m[k].(type) {
		case float64:
			return v, true
		case string:
			x, err := strconv.ParseFloat(v, 64)
			return x, err == nil
		}
		return 0, false
	}
	s := devsSnap{ok: true, at: at}
	for _, d := range r.DEVS {
		mh, ok := f(d, "Total MH")
		if !ok {
			return devsSnap{}
		}
		el, _ := f(d, "Device Elapsed")
		asc, _ := f(d, "ASC")
		id, hasID := f(d, "ID")
		e := devEntry{asc: int(asc), enabled: true, mh: mh, el: el}
		e.d1, e.hasD1 = f(d, "Diff1 Work")
		if hasID {
			e.id = strconv.Itoa(int(id))
		}
		if en, ok := d["Enabled"].(string); ok && en != "Y" {
			e.enabled = false
		}
		s.devs = append(s.devs, e)
	}
	return s
}

// readDevs: one devs read now (!ok = port 4028 unavailable or an unreadable reply).
func readDevs() devsSnap {
	at := time.Now()
	raw, err := cgCommand("devs")
	if err != nil {
		return devsSnap{}
	}
	return parseDevs(raw, at)
}

// byBoard: the snapshot's entries keyed by the wizard's board ids (boards = the enabled boards, as the REST API
// names them). The devs "ID" when it names every board, else the enabled entries in ASC order onto the sorted
// board ids when the counts match. nil = no safe mapping.
func (s devsSnap) byBoard(boards []string) map[string]devEntry {
	ids := map[string]devEntry{}
	for _, d := range s.devs {
		if d.id != "" {
			ids[d.id] = d
		}
	}
	out := map[string]devEntry{}
	for _, b := range boards {
		if d, ok := ids[b]; ok {
			out[b] = d
		}
	}
	if len(out) == len(boards) {
		return out
	}
	var en []devEntry
	for _, d := range s.devs {
		if d.enabled {
			en = append(en, d)
		}
	}
	if len(en) != len(boards) {
		return nil
	}
	sort.Slice(en, func(i, j int) bool { return en[i].asc < en[j].asc })
	bs := append([]string(nil), boards...)
	sort.Strings(bs)
	out = map[string]devEntry{}
	for i, b := range bs {
		out[b] = en[i]
	}
	return out
}

// errCounterReset: a board's Total MH (or Device Elapsed) went backwards between the window's two reads: bosminer
// restarted under the window, which is invalid (a hold is thrown out and redone like a "Setting voltage").
type errCounterReset struct{ board string }

func (e errCounterReset) Error() string {
	return fmt.Sprintf("bosminer's hash counter of board %s went backwards (restart): window invalid", e.board)
}

// devsFallback: why the last window used the board rates ("" = Total MH). Each reason is logged once.
var (
	devsNotedMu sync.Mutex
	devsNoted   = map[string]bool{}
)

func devsNote(why string) {
	devsNotedMu.Lock()
	defer devsNotedMu.Unlock()
	if !devsNoted[why] {
		devsNoted[why] = true
		note("wizard: cgminer API devs: %s - window hashrate from the board rates (API 1-min mean) instead", why)
	}
}

// exactRates: per-board TH/s and the total over the window between devs reads a and b. fb / fbTot = the window's
// board-rate summary (windowRates): its boards are the ones measured, and it is the fallback (src "boards") when
// port 4028 is unavailable or the reply cannot be mapped. A counter that went backwards -> errCounterReset (the
// fallback values are still returned). src = "devs" | "boards".
func exactRates(a, b devsSnap, fb map[string]float64, fbTot float64) (map[string]float64, float64, string, error) {
	why := ""
	boards := make([]string, 0, len(fb))
	for bd := range fb {
		boards = append(boards, bd)
	}
	var ma, mb map[string]devEntry
	dt := b.at.Sub(a.at).Seconds()
	switch {
	case !a.ok || !b.ok:
		why = "port 4028 unavailable"
	case dt <= 0:
		why = "no time between the reads"
	default:
		ma, mb = a.byBoard(boards), b.byBoard(boards)
		if ma == nil || mb == nil {
			why = "boards do not match the API's"
		}
	}
	if why != "" {
		devsNote(why)
		return fb, fbTot, "boards", nil
	}
	for _, bd := range boards { // a restart under the window on any board first: the window is invalid
		if mb[bd].mh < ma[bd].mh || mb[bd].el < ma[bd].el || (mb[bd].hasD1 && ma[bd].hasD1 && mb[bd].d1 < ma[bd].d1) {
			return fb, fbTot, "boards", errCounterReset{bd}
		}
	}
	out, tot := map[string]float64{}, 0.0
	for _, bd := range boards {
		d := mb[bd].mh - ma[bd].mh
		if mb[bd].hasD1 && ma[bd].hasD1 { // the smooth counter (see the file comment)
			d = (mb[bd].d1 - ma[bd].d1) * diff1Hashes / 1e6
		}
		th := d / dt / 1e6
		if fb[bd] > 1 && (th < 0.75*fb[bd] || th > 1.33*fb[bd]) { // a wrong board mapping or unit, not a real gap
			devsNote("a board's counter rate is more than 25 % off its board rate (board mapping or unit?)")
			return fb, fbTot, "boards", nil
		}
		out[bd] = math.Round(th*1000) / 1000
		tot += th
	}
	return out, math.Round(tot*1000) / 1000, "devs", nil
}
