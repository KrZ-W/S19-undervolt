package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The requested level survives a restart: saved with the core config, read back by loadCoreConfig.
func TestRequestedLevelSurvivesRestart(t *testing.T) {
	oldC, oldP := ccfg, ccfgPath
	defer func() { ccfg, ccfgPath = oldC, oldP }()
	p := filepath.Join(t.TempDir(), "core.json")
	if err := os.WriteFile(p, []byte(`{"fan":"live","trim":"live","power":"live","levels":{"2000":{"v":11.63,"d":36.78,"t":2000}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loadCoreConfig(p)
	ccfgPath = p
	ccfg.Requested = 2000
	if err := saveCoreConfig(""); err != nil {
		t.Fatal(err)
	}
	ccfg = coreConfig{}
	loadCoreConfig(p)
	if ccfg.Requested != 2000 || ccfg.Levels["2000"].T != 2000 {
		t.Fatalf("after reload: requested %d, levels %v", ccfg.Requested, ccfg.Levels)
	}
}
