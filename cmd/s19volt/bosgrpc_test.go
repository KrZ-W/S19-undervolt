package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

// leafPaths: every non-null leaf of a JSON value as "a.b[].c=<type>" (list elements merged).
func leafPaths(v interface{}, at string, out map[string]string) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, e := range t {
			leafPaths(e, at+"."+k, out)
		}
	case []interface{}:
		for _, e := range t {
			leafPaths(e, at+"[]", out)
		}
	case nil:
	default:
		out[at] = fmt.Sprintf("%T", t)
	}
}

// TestGRPCMatchesREST: answers captured from the same miner (BOS+ 26.11, both APIs, 2026-11-27) — the gRPC
// answer converted by the backend has every leaf the REST gateway sends, with the same JSON type.
func TestGRPCMatchesREST(t *testing.T) {
	for _, c := range []struct{ name, rpc string }{
		{"hashboards", "braiins.bos.v1.MinerService/GetHashboards"},
		{"stats", "braiins.bos.v1.MinerService/GetMinerStats"},
		{"cooling", "braiins.bos.v1.CoolingService/GetCoolingState"},
		{"constraints", "braiins.bos.v1.ConfigurationService/GetConstraints"},
		{"tuner", "braiins.bos.v1.PerformanceService/GetTunerState"},
	} {
		_, out, err := bosMethod(c.rpc)
		if err != nil {
			t.Fatal(err)
		}
		gj, _ := os.ReadFile("testdata/bosapi/" + c.name + ".grpc.json")
		m := dynamicpb.NewMessage(out)
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(gj, m); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		raw, _ := proto.Marshal(m)
		got, err := grpcAnswerToREST(out, raw, "")
		if err != nil {
			t.Fatal(err)
		}
		var g, r interface{}
		_ = json.Unmarshal(got, &g)
		rj, _ := os.ReadFile("testdata/bosapi/" + c.name + ".rest.json")
		_ = json.Unmarshal(rj, &r)
		gp, rp := map[string]string{}, map[string]string{}
		leafPaths(g, "", gp)
		leafPaths(r, "", rp)
		var missing []string
		for p, typ := range rp {
			if notIn140(p) {
				continue
			}
			if gp[p] != typ {
				missing = append(missing, fmt.Sprintf("%s (rest %s, grpc %q)", p, typ, gp[p]))
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s: %d REST leaves differ:\n  %s", c.name, len(missing), strings.Join(missing, "\n  "))
		}
	}
}

// REST fields newer than API 1.4.0 (Braiins OS 25.01): absent on the gRPC backend there. s19volt must not use
// them (TestNoNewerFieldsUsed).
var newerThan140 = []string{"chips_count", "chip_type", "serial_number", "highest_outlet_temp", "lowest_inlet_temp",
	"best_share_str", "fan_pause_runtime_limited_duration_s"}

func notIn140(path string) bool {
	for _, f := range newerThan140 {
		if strings.Contains(path, "."+f) {
			return true
		}
	}
	return false
}

func TestNoNewerFieldsUsed(t *testing.T) {
	files, _ := os.ReadDir(".")
	ui, _ := os.ReadDir("ui")
	var paths []string
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".go") && !strings.HasSuffix(f.Name(), "_test.go") {
			paths = append(paths, f.Name())
		}
	}
	for _, f := range ui {
		paths = append(paths, "ui/"+f.Name())
	}
	for _, p := range paths {
		b, _ := os.ReadFile(p)
		for _, f := range newerThan140 {
			if strings.Contains(string(b), f) {
				t.Errorf("%s uses %q, which Braiins OS 25.01 (API 1.4.0) does not send", p, f)
			}
		}
	}
}

// The fields s19volt reads, through the converted answers.
func TestGRPCFieldsCallersRead(t *testing.T) {
	conv := func(name, rpc string) json.RawMessage {
		_, out, _ := bosMethod(rpc)
		gj, _ := os.ReadFile("testdata/bosapi/" + name + ".grpc.json")
		m := dynamicpb.NewMessage(out)
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(gj, m); err != nil {
			t.Fatal(err)
		}
		raw, _ := proto.Marshal(m)
		js, _ := grpcAnswerToREST(out, raw, "")
		return js
	}
	tu := conv("tuner", "braiins.bos.v1.PerformanceService/GetTunerState")
	if w := num(tu, "mode_state", "powertargetmodestate", "current_target", "watt"); w != 2000 {
		t.Errorf("tuner current_target = %v", w)
	}
	var hb struct {
		Hashboards []map[string]interface{} `json:"hashboards"`
	}
	_ = json.Unmarshal(conv("hashboards", "braiins.bos.v1.MinerService/GetHashboards"), &hb)
	if len(hb.Hashboards) != 3 {
		t.Fatalf("boards %d", len(hb.Hashboards))
	}
	b := hb.Hashboards[0]
	if id, _ := b["id"].(string); id != "1" {
		t.Errorf("id %v", b["id"])
	}
	if en, _ := b["enabled"].(bool); !en {
		t.Errorf("enabled %v", b["enabled"])
	}
	if v := num(b, "stats", "real_hashrate", "last_1m", "gigahash_per_second"); v < 1000 {
		t.Errorf("last_1m %v", v)
	}
	if v := num(b, "highest_chip_temp", "temperature", "degree_c"); v < 20 {
		t.Errorf("chip temp %v", v)
	}
}

// Requests: the REST bodies s19volt sends become valid request messages.
func TestGRPCRequests(t *testing.T) {
	for _, c := range []struct {
		method, path string
		body         interface{}
		want         string // protojson of the request
	}{
		{"PUT", "/performance/power-target", map[string]int{"watt": 1300},
			`{"saveAction":"SAVE_ACTION_SAVE_AND_APPLY","powerTarget":{"watt":"1300"}}`},
		{"PATCH", "/miner/hw/hashboards", map[string]interface{}{"enable": false, "hashboard_ids": []string{"2", "3"}},
			`{"saveAction":"SAVE_ACTION_SAVE_AND_APPLY","hashboardIds":["2","3"]}`},
		{"PUT", "/performance/mode", manualModeBody(250, 11.96),
			`{"saveAction":"SAVE_ACTION_SAVE_AND_APPLY","mode":{"manualMode":{"globalFrequency":{"hertz":250000000},"globalVoltage":{"volt":11.96}}}}`},
		{"PUT", "/performance/mode", tunerModeBody(2000),
			`{"saveAction":"SAVE_ACTION_SAVE_AND_APPLY","mode":{"tunerMode":{"powerTarget":{"powerTarget":{"watt":"2000"}}}}}`},
		{"PUT", "/actions/pause", nil, `{}`},
	} {
		var obj map[string]interface{}
		if c.body != nil {
			js, _ := json.Marshal(c.body)
			_ = json.Unmarshal(js, &obj)
		}
		rt, err := bosRouteFor(c.method, c.path, obj)
		if err != nil {
			t.Fatal(err)
		}
		in, _, err := bosMethod(rt.rpc)
		if err != nil {
			t.Fatal(err)
		}
		if rt.req != nil {
			obj = rt.req(obj)
		}
		m := dynamicpb.NewMessage(in)
		if err := restToMsg(m, obj); err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		got, _ := protojson.Marshal(m)
		var g, w interface{}
		_ = json.Unmarshal(got, &g)
		_ = json.Unmarshal([]byte(c.want), &w)
		gs, _ := json.Marshal(g)
		ws, _ := json.Marshal(w)
		if string(gs) != string(ws) {
			t.Errorf("%s %s:\n got %s\nwant %s", c.method, c.path, gs, ws)
		}
	}
	if rt, _ := bosRouteFor("PATCH", "/miner/hw/hashboards", map[string]interface{}{"enable": true}); !strings.HasSuffix(rt.rpc, "/EnableHashboards") {
		t.Errorf("enable -> %s", rt.rpc)
	}
}
