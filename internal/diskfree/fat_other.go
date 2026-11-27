//go:build !linux && !darwin && !windows

package diskfree

// IsFAT is not detected on this system; split the output explicitly when
// writing to a FAT card.
func IsFAT(string) (bool, error) { return false, nil }
