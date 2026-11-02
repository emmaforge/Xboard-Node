package main

import (
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
)

func TestParseMemLimitStrict(t *testing.T) {
	// Regression test for B15: Sscanf("%d") used to accept "1.5" as
	// 1 and "12abc" as 12, silently applying the wrong memory limit.
	valid := []struct {
		in   string
		want int64
	}{
		{"512MiB", 512 << 20},
		{"2 GiB", 2 << 30},
		{"1024", 1024},
		{"1KiB", 1 << 10},
		{"1tib", 1 << 40},
	}
	for _, tc := range valid {
		got, err := parseMemLimit(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseMemLimit(%q) = %d, %v, want %d", tc.in, got, err, tc.want)
		}
	}
	invalid := []string{"1.5GiB", "12abc", "abc", "", "-1GiB", "999999999999TiB", "1.5"}
	for _, in := range invalid {
		if got, err := parseMemLimit(in); err == nil {
			t.Errorf("parseMemLimit(%q) = %d, want error", in, got)
		}
	}
}

func TestCountServices(t *testing.T) {
	nodeInst := func(nodes ...int) *config.Config {
		c := &config.Config{}
		for _, id := range nodes {
			c.Nodes = append(c.Nodes, config.NodeEntry{NodeID: id})
		}
		return c
	}
	machineInst := func() *config.Config {
		return &config.Config{Machine: &config.MachineConfig{MachineID: 1}}
	}
	// Single instance, three nodes: three services, not one. The old
	// errCh sizing (len(instances) = 1) deadlocked exactly this case.
	if got := countServices([]*config.Config{nodeInst(1, 2, 3)}); got != 3 {
		t.Fatalf("single multi-node instance: got %d, want 3", got)
	}
	if got := countServices([]*config.Config{nodeInst(5), machineInst()}); got != 2 {
		t.Fatalf("node + machine instances: got %d, want 2", got)
	}
	if got := countServices(nil); got != 1 {
		t.Fatalf("empty instances: got %d, want >= 1", got)
	}
}

func TestWarnFirstInstanceOnlyDoesNotPanic(t *testing.T) {
	first := &config.Config{
		HealthPort: 65530,
		Log:        config.LogConfig{Level: "info", Output: "stdout"},
		Runtime:    config.RuntimeConfig{GoMemLimit: "256MiB"},
	}
	second := &config.Config{
		HealthPort: 65531,
		Log:        config.LogConfig{Level: "debug", Output: "stdout"},
		Runtime:    config.RuntimeConfig{GoMemLimit: "128MiB"},
	}
	// Smoke test: differing later-instance settings must be handled
	// (warned about) without panicking, including empty input.
	warnFirstInstanceOnly([]*config.Config{first, second})
	warnFirstInstanceOnly([]*config.Config{first})
	warnFirstInstanceOnly(nil)
}
