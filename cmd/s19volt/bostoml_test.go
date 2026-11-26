package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tomlSample = `[format]
version = "2.0"

[hash_chain.1]
enabled = true

[temp_control]
mode = "manual"
target_temp = 75.0

[autotuning]
enabled = true
mode = "power_target"
power_target = 1260

[performance_scaling]
enabled = false
`

func TestSetAutotuneTargetReplaces(t *testing.T) {
	out, err := setAutotuneTarget(tomlSample, 1170)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(tomlSample, "power_target = 1260", "power_target = 1170", 1)
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	// other sections untouched: [temp_control]'s mode stays manual
	if !strings.Contains(out, "[temp_control]\nmode = \"manual\"") {
		t.Fatal("temp_control changed")
	}
	again, err := setAutotuneTarget(out, 1170)
	if err != nil || again != out {
		t.Fatalf("not idempotent: %v\n%s", err, again)
	}
}

func TestSetAutotuneTargetAddsMissingKeys(t *testing.T) {
	src := "[autotuning]\nenabled = true\n\n[performance_scaling]\nenabled = false\n"
	out, err := setAutotuneTarget(src, 900)
	if err != nil {
		t.Fatal(err)
	}
	want := "[autotuning]\nenabled = true\nmode = \"power_target\"\npower_target = 900\n\n[performance_scaling]\nenabled = false\n"
	if out != want {
		t.Fatalf("got %q", out)
	}
	if again, _ := setAutotuneTarget(out, 900); again != out {
		t.Fatalf("not idempotent: %q", again)
	}
	// section last in the file, other mode fixed
	out, err = setAutotuneTarget("[autotuning]\nmode = \"hashrate_target\"\n  power_target = 2200 # x\n", 1500)
	if err != nil || out != "[autotuning]\nmode = \"power_target\"\n  power_target = 1500\n" {
		t.Fatalf("got %q %v", out, err)
	}
}

func TestSetAutotuneTargetRefuses(t *testing.T) {
	for name, src := range map[string]string{
		"absent":    "[temp_control]\nmode = \"manual\"\n",
		"twice":     "[autotuning]\npower_target = 1\n[autotuning]\npower_target = 2\n",
		"not plain": "[autotuning]\npower_target = { watt = 900 }\n",
		"commented": "#[autotuning]\npower_target = 900\n",
	} {
		out, err := setAutotuneTarget(src, 1000)
		if err == nil || out != src {
			t.Errorf("%s: want refusal, got %q %v", name, out, err)
		}
	}
	if _, err := setAutotuneTarget(tomlSample, 0); err == nil {
		t.Error("0 W accepted")
	}
}

func TestWriteAutotuneTarget(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bosminer.toml")
	oldBak := bosTomlBackup
	bosTomlBackup = filepath.Join(dir, "bak")
	defer func() { bosTomlBackup = oldBak }()
	if err := os.WriteFile(p, []byte(tomlSample), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAutotuneTarget(p, 1170); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "power_target = 1170") {
		t.Fatalf("not written: %s", b)
	}
	if bak, _ := os.ReadFile(bosTomlBackup); string(bak) != tomlSample {
		t.Fatal("backup is not the original")
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if err := os.WriteFile(p, []byte("[x]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAutotuneTarget(p, 1170); err == nil {
		t.Fatal("missing section accepted")
	}
	if b, _ := os.ReadFile(p); string(b) != "[x]\n" {
		t.Fatal("file changed on refusal")
	}
}
