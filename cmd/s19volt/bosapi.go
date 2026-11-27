package main

// Braiins API access. Supported firmware: Braiins OS+ 25.01 (API 1.4.0), gRPC on 127.0.0.1:50051 (bosgrpc.go).
// Every call in s19volt goes through bos(method, path, body) with REST-style paths and REST-shaped JSON (the
// shape Braiins' later REST gateway uses): the gRPC answer is converted to it, so callers read one familiar shape.

import (
	"encoding/json"
	"sync"
)

var (
	bosOnce sync.Once
	bosImpl *grpcBackend
)

// bos: one Braiins API call (REST-style path + JSON body), carried over gRPC.
func bos(method, path string, body interface{}) (json.RawMessage, error) {
	bosOnce.Do(func() { bosImpl = newGRPCBackend("127.0.0.1:50051") })
	return bosImpl.call(method, path, body)
}
