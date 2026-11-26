package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

var sizeUnits = map[string]float64{
	"": 1, "b": 1,
	"k": 1 << 10, "kib": 1 << 10, "kb": 1e3,
	"m": 1 << 20, "mib": 1 << 20, "mb": 1e6,
	"g": 1 << 30, "gib": 1 << 30, "gb": 1e9,
	"t": 1 << 40, "tib": 1 << 40, "tb": 1e12,
}

// parseSize parses a byte count such as 268435456, 256MiB, 64M or 1.5GB.
// K/M/G/T and KiB/MiB/GiB/TiB are binary multiples; KB/MB/GB/TB are decimal.
func parseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	i := 0
	for i < len(t) && (t[i] >= '0' && t[i] <= '9' || t[i] == '.') {
		i++
	}
	n, err := strconv.ParseFloat(t[:i], 64)
	unit, ok := sizeUnits[strings.ToLower(strings.TrimSpace(t[i:]))]
	if err != nil || !ok || n < 0 || math.IsInf(n*unit, 0) || n*unit >= math.MaxInt64 {
		return 0, fmt.Errorf("invalid size %q (examples: 268435456, 256MiB, 64M, 1.5GB)", s)
	}
	return int64(math.Round(n * unit)), nil
}

// humanBytes formats a byte count with binary units for progress lines.
func humanBytes(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	if n < 1024 {
		return fmt.Sprintf("%s%d B", sign, n)
	}
	v := float64(n)
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB"} {
		v /= 1024
		if v < 1024 || unit == "TiB" {
			return fmt.Sprintf("%s%.1f %s", sign, v, unit)
		}
	}
	return ""
}
