package main

import (
	"strings"
	"testing"
)

func TestParseConfigSlots(t *testing.T) {
	cfg, err := parseConfig(strings.NewReader("remote b 10.0.0.2\nremote a 10.0.0.1\nslots a 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.order, ",") != "b,a" {
		t.Errorf("order = %v, want config order b,a", cfg.order)
	}
	if cfg.slots["a"] != 3 || cfg.slots["b"] != 0 {
		t.Errorf("slots = %v", cfg.slots)
	}
	if _, err := parseConfig(strings.NewReader("slots a x")); err == nil {
		t.Error("a non-numeric slot count was accepted")
	}
}
