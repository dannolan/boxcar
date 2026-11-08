//go:build windows

package nuptools

import "os"

// fileIno returns 0 on Windows (no portable inode number).
func fileIno(fi os.FileInfo) uint64 { return 0 }
