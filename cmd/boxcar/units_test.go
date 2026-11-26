package main

import "testing"

func TestParseSize(t *testing.T) {
	good := map[string]int64{
		"268435456": 268435456,
		"256MiB":    256 << 20,
		"256mib":    256 << 20,
		"64M":       64 << 20,
		"1.5GiB":    3 << 29,
		"1GB":       1000000000,
		"4 KB":      4000,
		"512k":      512 << 10,
		"0":         0,
	}
	for in, want := range good {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MiB", "-1", "12XB", "1e3", "9999999TB"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q): want error", in)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 3 << 29: "1.5 GiB", -2048: "-2.0 KiB",
	} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
