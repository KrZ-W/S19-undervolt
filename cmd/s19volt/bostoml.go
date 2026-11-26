package main

// Line-based edit of bosminer.toml's [autotuning] section, used while bosminer is stopped so it starts directly at
// a power target (the wizard's level start: no tune of the previous target from scratch first). Only the
// [autotuning] section's mode and power_target lines change; everything else is copied byte for byte.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var bosTomlFile = "/etc/bosminer.toml"           // var: tests point it at a copy
var bosTomlBackup = "/tmp/bosminer.toml.s19volt" // copy of the file before each edit

var errNoAutotuning = errors.New("bosminer.toml has no [autotuning] section")

// tomlKey: the key of a "key = value" line (trimmed), "" for anything else.
func tomlKey(line string) string {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "[") {
		return ""
	}
	i := strings.Index(t, "=")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(t[:i])
}

// tomlValue: the value of a "key = value" line, without a trailing comment.
func tomlValue(line string) string {
	v := line[strings.Index(line, "=")+1:]
	if i := strings.Index(v, "#"); i >= 0 && !strings.Contains(v[:i], `"`) {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func isTomlHeader(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "[") }

// setAutotuneTarget returns src with [autotuning] set to mode = "power_target" and power_target = watts (missing
// keys are added at the end of the section). Refuses (error, src unchanged) when the section is missing or
// repeated, or when power_target is not a plain integer (a format this edit does not know).
func setAutotuneTarget(src string, watts int) (string, error) {
	if watts <= 0 {
		return src, fmt.Errorf("power target %d W", watts)
	}
	lines := strings.Split(src, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "[autotuning]" {
			if start >= 0 {
				return src, errors.New("bosminer.toml has more than one [autotuning] section")
			}
			start = i
		}
	}
	if start < 0 {
		return src, errNoAutotuning
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if isTomlHeader(lines[i]) {
			end = i
			break
		}
	}
	indent := func(l string) string { return l[:len(l)-len(strings.TrimLeft(l, " \t"))] }
	seenMode, seenTarget, last := false, false, start
	for i := start + 1; i < end; i++ {
		switch tomlKey(lines[i]) {
		case "mode":
			lines[i], seenMode = indent(lines[i])+`mode = "power_target"`, true
		case "power_target":
			if _, err := strconv.Atoi(tomlValue(lines[i])); err != nil {
				return src, fmt.Errorf("[autotuning] power_target is not a plain number: %q", strings.TrimSpace(lines[i]))
			}
			lines[i], seenTarget = indent(lines[i])+fmt.Sprintf("power_target = %d", watts), true
		}
		if strings.TrimSpace(lines[i]) != "" {
			last = i
		}
	}
	var add []string
	if !seenMode {
		add = append(add, `mode = "power_target"`)
	}
	if !seenTarget {
		add = append(add, fmt.Sprintf("power_target = %d", watts))
	}
	if len(add) > 0 { // after the section's last non-blank line (blank lines before the next section stay)
		lines = append(lines[:last+1], append(add, lines[last+1:]...)...)
	}
	return strings.Join(lines, "\n"), nil
}

// writeAutotuneTarget: setAutotuneTarget on the file (bosminer must be stopped). The original is copied to
// bosTomlBackup first; the new file replaces it atomically with the same permissions. No change = no write.
func writeAutotuneTarget(path string, watts int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, err := setAutotuneTarget(string(b), watts)
	if err != nil {
		return err
	}
	if out == string(b) {
		return nil
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.WriteFile(bosTomlBackup, b, mode); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".s19volt-new")
	if err := os.WriteFile(tmp, []byte(out), mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// configuredTarget: the power target bosminer is configured for (GET /performance/mode), 0 if unknown.
func configuredTarget() int {
	raw, err := bos("GET", "/performance/mode", nil)
	if err != nil {
		return 0
	}
	return parseModeTarget(raw)
}
