package main

import (
	"testing"
	"time"
)

func TestControllerState(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if k := controllerState("wizard", "a", now, now)["kind"]; k != "setup" {
		t.Errorf("setup: %v", k)
	}
	c := controllerState("", "client", now.Add(-12*time.Minute), now)
	if c["kind"] != "api" || c["age_min"] != 12 || c["addr"] != "client" {
		t.Errorf("recent call: %v", c)
	}
	if k := controllerState("", "client", now.Add(-61*time.Minute), now)["kind"]; k != "manual" {
		t.Errorf("old call: %v", k)
	}
	if k := controllerState("", "", time.Time{}, now)["kind"]; k != "manual" {
		t.Errorf("never called: %v", k)
	}
}
