package main

import (
	"testing"
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
