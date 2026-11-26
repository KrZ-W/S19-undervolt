package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestUIScriptsParse: every inline <script> in the embedded pages and every ui/*.js must parse. A single bad quote
// in a help string broke the whole setup monitor once (2026-11-25). Skipped when node is not installed.
func TestUIScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	re := regexp.MustCompile(`(?s)<script(?:\s[^>]*)?>(.*?)</script>`)
	files, _ := filepath.Glob("ui/*.html")
	js, _ := filepath.Glob("ui/*.js")
	dir := t.TempDir()
	check := func(name, src string) {
		p := filepath.Join(dir, strings.ReplaceAll(name, "/", "_")+".js")
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", p).CombinedOutput(); err != nil {
			t.Errorf("%s: script does not parse:\n%s", name, out)
		}
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if strings.TrimSpace(m[1]) != "" {
				parts = append(parts, m[1])
			}
		}
		check(f, strings.Join(parts, "\n"))
	}
	for _, f := range js {
		b, _ := os.ReadFile(f)
		check(f, string(b))
	}
}
