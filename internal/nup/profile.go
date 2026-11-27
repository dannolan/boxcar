package nup

import (
	"fmt"
	"sort"
	"strings"
)

// LevelBest is the optimizer-only zstd level for the Archive profile
// (klauspost's SpeedBestCompression). Pack keeps the reference's 1, 3 and 9.
const LevelBest = 19

// RuntimeLargeFile is the Runtime profile's large-file threshold. nx2-flow's
// loader (0.1.7) unpacks a compressed file whole on first read into a cache
// of max(192 MiB, largest compressed file's size + 64 MiB); keeping every
// compressed file at or under 128 MiB keeps that cache at its 192 MiB
// minimum. The loader streams the frame into the file's memory, so the
// frame's own size does not matter; the file's size does.
const RuntimeLargeFile = 128 << 20

// Profiles are the optimizer's named storage policies. They are only
// presets of RepackOptions: the CLI starts from one and explicit flags
// override single fields, so there is exactly one policy implementation.
var Profiles = map[string]RepackOptions{
	// Runtime: archives loaded directly by nx2-flow. Balanced compression,
	// files over 128 MiB raw, a file is compressed only when zstd saves at
	// least 5% of it (anything less costs the console a full unpack into
	// its cache for almost no space), existing frames reused.
	"runtime": {Level: 3, LargeRaw: true, LargeFileThreshold: RuntimeLargeFile, MinSavings: 5},
	// Archive: storage only. Strongest level, no large-file rule, any saving
	// kept, existing frames re-encoded when that is smaller.
	"archive": {Level: LevelBest, LargeRaw: false, MinSavings: 0, Recompress: true},
	// Fast: throughput first. Fastest level, reference large-file rule,
	// frames reused; deduplication and verification as always.
	"fast": {Level: 1, LargeRaw: true, LargeFileThreshold: LargeFile, MinSavings: 0},
}

// DefaultProfile is the optimizer's default.
const DefaultProfile = "runtime"

// ProfileOptions returns the named profile (case-insensitive).
func ProfileOptions(name string) (RepackOptions, error) {
	if p, ok := Profiles[strings.ToLower(name)]; ok {
		return p, nil
	}
	names := make([]string, 0, len(Profiles))
	for n := range Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return RepackOptions{}, fmt.Errorf("unknown profile %q (%s)", name, strings.Join(names, ", "))
}

// OptimizeLevels lists the zstd levels the optimizer accepts.
func validOptimizeLevel(level int) bool {
	return level == 1 || level == 3 || level == 9 || level == LevelBest
}
