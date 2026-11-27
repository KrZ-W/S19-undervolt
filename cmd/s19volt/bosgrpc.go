package main

// gRPC client for Braiins OS+ 25.01 (API 1.4.0), the supported firmware (the last stable SD release for the S19).
//
// No grpc-go (several MB on a ~55 MB overlay): a unary gRPC call is one HTTP/2 POST with a 5-byte frame header,
// which Go's net/http does over cleartext HTTP/2 itself. Messages are built and read with the protobuf runtime
// from the API definitions embedded below (bos-plus-api tag 1.4.0, compiled with grpcurl -protoset-out).
//
// Callers speak REST (paths + REST JSON). The REST gateway's JSON is the same messages with: proto field names
// (snake_case), enums as numbers, 64-bit integers as numbers, and each oneof as {oneof_name: {variant: value}}
// where variant = the field name lower-cased without underscores (GetTunerState: "mode_state":
// {"powertargetmodestate": ...}). GET /performance/mode is the exception: it answers the PerformanceMode oneof
// without its wrapper ({"tunermode": ...}), and PUT takes the same. Verified side by side on BOS+ 26.11
// (2026-11-27), which serves both APIs.

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

//go:embed bosapi-1.4.0.protoset
var bosProtoset []byte

const saveAndApply = 2 // braiins.bos.v1.SaveAction SAVE_ACTION_SAVE_AND_APPLY (what the REST gateway does)

// bosRoute: one REST call mapped to its gRPC method. req turns the REST body into the request message's JSON
// (REST shape); unwrap names a top-level oneof the REST answer leaves out.
type bosRoute struct {
	rpc    string
	req    func(body map[string]interface{}) map[string]interface{}
	unwrap string
}

func withSave(field string) func(map[string]interface{}) map[string]interface{} {
	return func(b map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"save_action": saveAndApply, field: b}
	}
}

var bosRoutes = map[string]bosRoute{
	"GET /miner/hw/hashboards":       {rpc: "braiins.bos.v1.MinerService/GetHashboards"},
	"GET /miner/stats":               {rpc: "braiins.bos.v1.MinerService/GetMinerStats"},
	"GET /cooling/state":             {rpc: "braiins.bos.v1.CoolingService/GetCoolingState"},
	"GET /configuration/constraints": {rpc: "braiins.bos.v1.ConfigurationService/GetConstraints"},
	"GET /performance/tuner-state":   {rpc: "braiins.bos.v1.PerformanceService/GetTunerState"},
	"GET /performance/mode":          {rpc: "braiins.bos.v1.PerformanceService/GetActivePerformanceMode", unwrap: "mode"},
	"PUT /actions/pause":             {rpc: "braiins.bos.v1.ActionsService/PauseMining"},
	"PUT /actions/resume":            {rpc: "braiins.bos.v1.ActionsService/ResumeMining"},
	"PUT /performance/power-target":  {rpc: "braiins.bos.v1.PerformanceService/SetPowerTarget", req: withSave("power_target")},
	"PUT /performance/mode":          {rpc: "braiins.bos.v1.PerformanceService/SetPerformanceMode", req: withSave("mode")},
	"PATCH /miner/hw/hashboards":     {}, // Enable/DisableHashboards by body "enable" (bosRoute picks)
}

func bosRouteFor(method, path string, body map[string]interface{}) (bosRoute, error) {
	key := method + " " + path
	r, ok := bosRoutes[key]
	if !ok {
		return r, fmt.Errorf("%s: no gRPC mapping", key)
	}
	if key == "PATCH /miner/hw/hashboards" {
		rpc := "braiins.bos.v1.MinerService/DisableHashboards"
		if en, _ := body["enable"].(bool); en {
			rpc = "braiins.bos.v1.MinerService/EnableHashboards"
		}
		return bosRoute{rpc: rpc, req: func(b map[string]interface{}) map[string]interface{} {
			return map[string]interface{}{"save_action": saveAndApply, "hashboard_ids": b["hashboard_ids"]}
		}}, nil
	}
	return r, nil
}

// ---- API definitions ----

var (
	bosFilesOnce sync.Once
	bosFiles     *protoregistry.Files
	bosFilesErr  error
)

func bosDescriptors() (*protoregistry.Files, error) {
	bosFilesOnce.Do(func() {
		var set descriptorpb.FileDescriptorSet
		if bosFilesErr = proto.Unmarshal(bosProtoset, &set); bosFilesErr == nil {
			bosFiles, bosFilesErr = protodesc.NewFiles(&set)
		}
	})
	return bosFiles, bosFilesErr
}

// bosMethod: the request and response message descriptors of "pkg.Service/Method".
func bosMethod(rpc string) (in, out protoreflect.MessageDescriptor, err error) {
	files, err := bosDescriptors()
	if err != nil {
		return nil, nil, err
	}
	i := strings.LastIndex(rpc, "/")
	if i < 0 {
		return nil, nil, fmt.Errorf("bad rpc %q", rpc)
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName(rpc[:i]))
	if err != nil {
		return nil, nil, err
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, nil, fmt.Errorf("%s is not a service", rpc[:i])
	}
	md := sd.Methods().ByName(protoreflect.Name(rpc[i+1:]))
	if md == nil {
		return nil, nil, fmt.Errorf("%s: no such method", rpc)
	}
	return md.Input(), md.Output(), nil
}

// ---- protobuf message <-> REST-shaped JSON ----

func oneofVariant(fd protoreflect.FieldDescriptor) string {
	return strings.ReplaceAll(strings.ToLower(string(fd.Name())), "_", "")
}

// msgToREST: a message as the REST gateway's JSON value. Implicit-presence scalars are always present (REST
// sends 0/false); unset messages and oneofs are left out.
func msgToREST(m protoreflect.Message) map[string]interface{} {
	out := map[string]interface{}{}
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if o := fd.ContainingOneof(); o != nil && !o.IsSynthetic() {
			if !m.Has(fd) {
				continue
			}
			out[string(o.Name())] = map[string]interface{}{oneofVariant(fd): valueToREST(fd, m.Get(fd))}
			continue
		}
		if !m.Has(fd) && (fd.HasPresence() || fd.IsList() || fd.IsMap()) {
			if fd.IsList() {
				out[string(fd.Name())] = []interface{}{}
			}
			continue
		}
		out[string(fd.Name())] = valueToREST(fd, m.Get(fd))
	}
	return out
}

func valueToREST(fd protoreflect.FieldDescriptor, v protoreflect.Value) interface{} {
	switch {
	case fd.IsList():
		l := v.List()
		a := make([]interface{}, l.Len())
		for i := range a {
			a[i] = scalarToREST(fd, l.Get(i))
		}
		return a
	case fd.IsMap():
		mo := map[string]interface{}{}
		v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
			mo[k.String()] = scalarToREST(fd.MapValue(), mv)
			return true
		})
		return mo
	}
	return scalarToREST(fd, v)
}

func scalarToREST(fd protoreflect.FieldDescriptor, v protoreflect.Value) interface{} {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return msgToREST(v.Message())
	case protoreflect.EnumKind:
		return int(v.Enum())
	case protoreflect.BytesKind:
		return base64.StdEncoding.EncodeToString(v.Bytes())
	}
	return v.Interface() // bool, string, int32/64, uint32/64, float, double: JSON numbers/strings as REST
}

// restToMsg fills m from a REST-shaped JSON object. A key is a field name, a oneof name wrapping {variant: value},
// or a bare oneof variant (the unwrapped PerformanceMode form).
func restToMsg(m protoreflect.Message, obj map[string]interface{}) error {
	md := m.Descriptor()
	for k, v := range obj {
		if fd := md.Fields().ByName(protoreflect.Name(k)); fd != nil {
			if err := setREST(m, fd, v); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			continue
		}
		if o := md.Oneofs().ByName(protoreflect.Name(k)); o != nil {
			inner, ok := v.(map[string]interface{})
			if !ok || len(inner) != 1 {
				return fmt.Errorf("%s: oneof wants {variant: value}", k)
			}
			if err := restToMsg(m, inner); err != nil {
				return err
			}
			continue
		}
		fd := variantField(md, k)
		if fd == nil {
			return fmt.Errorf("%s: no field %q", md.FullName(), k)
		}
		if err := setREST(m, fd, v); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

func variantField(md protoreflect.MessageDescriptor, k string) protoreflect.FieldDescriptor {
	for i := 0; i < md.Oneofs().Len(); i++ {
		fs := md.Oneofs().Get(i).Fields()
		for j := 0; j < fs.Len(); j++ {
			if oneofVariant(fs.Get(j)) == k {
				return fs.Get(j)
			}
		}
	}
	return nil
}

func setREST(m protoreflect.Message, fd protoreflect.FieldDescriptor, v interface{}) error {
	if v == nil {
		return nil
	}
	switch {
	case fd.IsList():
		a, ok := v.([]interface{})
		if !ok {
			return fmt.Errorf("want a list")
		}
		l := m.Mutable(fd).List()
		for _, e := range a {
			if fd.Kind() == protoreflect.MessageKind {
				el := l.NewElement()
				obj, ok := e.(map[string]interface{})
				if !ok {
					return fmt.Errorf("want objects in the list")
				}
				if err := restToMsg(el.Message(), obj); err != nil {
					return err
				}
				l.Append(el)
				continue
			}
			pv, err := restScalar(fd, e)
			if err != nil {
				return err
			}
			l.Append(pv)
		}
		return nil
	case fd.IsMap():
		return fmt.Errorf("map fields not supported")
	case fd.Kind() == protoreflect.MessageKind:
		obj, ok := v.(map[string]interface{})
		if !ok {
			return fmt.Errorf("want an object")
		}
		return restToMsg(m.Mutable(fd).Message(), obj)
	}
	pv, err := restScalar(fd, v)
	if err != nil {
		return err
	}
	m.Set(fd, pv)
	return nil
}

func restScalar(fd protoreflect.FieldDescriptor, v interface{}) (protoreflect.Value, error) {
	switch n := v.(type) { // Go-built bodies carry ints; decoded JSON carries float64
	case int:
		v = float64(n)
	case int32:
		v = float64(n)
	case int64:
		v = float64(n)
	}
	f, isNum := v.(float64)
	s, isStr := v.(string)
	if isStr && fd.Kind() != protoreflect.StringKind && fd.Kind() != protoreflect.BytesKind && fd.Kind() != protoreflect.EnumKind {
		var err error
		if f, err = strconv.ParseFloat(s, 64); err != nil {
			return protoreflect.Value{}, fmt.Errorf("want a number, got %q", s)
		}
		isNum = true
	}
	switch fd.Kind() {
	case protoreflect.BoolKind:
		b, ok := v.(bool)
		if !ok {
			return protoreflect.Value{}, fmt.Errorf("want a bool")
		}
		return protoreflect.ValueOfBool(b), nil
	case protoreflect.StringKind:
		if !isStr {
			return protoreflect.Value{}, fmt.Errorf("want a string")
		}
		return protoreflect.ValueOfString(s), nil
	case protoreflect.BytesKind:
		b, err := base64.StdEncoding.DecodeString(s)
		return protoreflect.ValueOfBytes(b), err
	case protoreflect.EnumKind:
		if isStr {
			ev := fd.Enum().Values().ByName(protoreflect.Name(s))
			if ev == nil {
				return protoreflect.Value{}, fmt.Errorf("no enum value %q", s)
			}
			return protoreflect.ValueOfEnum(ev.Number()), nil
		}
		if !isNum {
			return protoreflect.Value{}, fmt.Errorf("want an enum number")
		}
		return protoreflect.ValueOfEnum(protoreflect.EnumNumber(int32(f))), nil
	}
	if !isNum {
		return protoreflect.Value{}, fmt.Errorf("want a number")
	}
	switch fd.Kind() {
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32(float32(f)), nil
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(f), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(int32(f)), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(int64(f)), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(uint32(f)), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(uint64(f)), nil
	}
	return protoreflect.Value{}, fmt.Errorf("unsupported kind %s", fd.Kind())
}

// ---- transport: unary gRPC over cleartext HTTP/2 ----

type grpcBackend struct {
	addr  string
	hc    *http.Client
	mu    sync.Mutex
	tok   string
	tokAt time.Time
}

func newGRPCBackend(addr string) *grpcBackend {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return &grpcBackend{addr: addr, hc: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Protocols: p}}}
}

type grpcStatusErr struct {
	code int
	msg  string
	rpc  string
}

func (e *grpcStatusErr) Error() string {
	return fmt.Sprintf("%s: grpc status %d %s", e.rpc, e.code, e.msg)
}

const grpcUnauthenticated = 16

// unary sends one request message and returns the response message bytes and headers.
func (g *grpcBackend) unary(rpc string, msg []byte, tok string) ([]byte, http.Header, error) {
	frame := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(msg)))
	copy(frame[5:], msg)
	req, err := http.NewRequest("POST", "http://"+g.addr+"/"+rpc, bytes.NewReader(frame))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	if tok != "" {
		req.Header.Set("authorization", tok)
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != 200 {
		return nil, nil, fmt.Errorf("%s: http %d", rpc, resp.StatusCode)
	}
	st := resp.Trailer.Get("grpc-status")
	stMsg := resp.Trailer.Get("grpc-message")
	if st == "" { // trailers-only answer (errors): status in the headers
		st, stMsg = resp.Header.Get("grpc-status"), resp.Header.Get("grpc-message")
	}
	if st != "" && st != "0" {
		code, _ := strconv.Atoi(st)
		return nil, nil, &grpcStatusErr{code: code, msg: stMsg, rpc: rpc}
	}
	if len(raw) < 5 {
		return nil, resp.Header, nil // empty message
	}
	if raw[0] != 0 {
		return nil, nil, fmt.Errorf("%s: compressed answer not supported", rpc)
	}
	n := binary.BigEndian.Uint32(raw[1:5])
	if int(n) > len(raw)-5 {
		return nil, nil, fmt.Errorf("%s: short answer", rpc)
	}
	return raw[5 : 5+n], resp.Header, nil
}

func (g *grpcBackend) login() (string, error) {
	in, _, err := bosMethod("braiins.bos.v1.AuthenticationService/Login")
	if err != nil {
		return "", err
	}
	m := dynamicpb.NewMessage(in)
	if err = restToMsg(m, map[string]interface{}{"username": wcfg.BosUser, "password": wcfg.BosPass}); err != nil {
		return "", err
	}
	b, _ := proto.Marshal(m)
	_, hdr, err := g.unary("braiins.bos.v1.AuthenticationService/Login", b, "")
	if err != nil {
		return "", fmt.Errorf("login failed: %w", err)
	}
	tok := hdr.Get("authorization")
	if tok == "" {
		return "", fmt.Errorf("login failed (no token)")
	}
	return tok, nil
}

func (g *grpcBackend) call(method, path string, body interface{}) (json.RawMessage, error) {
	var obj map[string]interface{}
	if body != nil { // REST body -> generic JSON object (callers pass maps and structs)
		js, _ := json.Marshal(body)
		if err := json.Unmarshal(js, &obj); err != nil {
			return nil, fmt.Errorf("%s %s: body: %w", method, path, err)
		}
	}
	rt, err := bosRouteFor(method, path, obj)
	if err != nil {
		return nil, err
	}
	in, out, err := bosMethod(rt.rpc)
	if err != nil {
		return nil, err
	}
	reqObj := obj
	if rt.req != nil {
		reqObj = rt.req(obj)
	}
	m := dynamicpb.NewMessage(in)
	if err = restToMsg(m, reqObj); err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	reqBytes, err := proto.Marshal(m)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		g.mu.Lock()
		if g.tok == "" || time.Since(g.tokAt) > 30*time.Minute || attempt == 1 {
			t, err := g.login()
			if err != nil {
				g.mu.Unlock()
				return nil, err
			}
			g.tok, g.tokAt = t, time.Now()
		}
		tok := g.tok
		g.mu.Unlock()
		raw, _, err := g.unary(rt.rpc, reqBytes, tok)
		if se, ok := err.(*grpcStatusErr); ok && se.code == grpcUnauthenticated && attempt == 0 {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", method, path, err)
		}
		return grpcAnswerToREST(out, raw, rt.unwrap)
	}
	return nil, fmt.Errorf("%s %s: unauthorized", method, path)
}

// grpcAnswerToREST: response bytes -> the REST gateway's JSON.
func grpcAnswerToREST(md protoreflect.MessageDescriptor, raw []byte, unwrap string) (json.RawMessage, error) {
	m := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(raw, m); err != nil {
		return nil, err
	}
	var v interface{} = msgToREST(m)
	if unwrap != "" {
		if inner, ok := v.(map[string]interface{})[unwrap]; ok {
			v = inner
		} else {
			v = map[string]interface{}{}
		}
	}
	return json.Marshal(v)
}
