//go:build freebsd || openbsd || netbsd || dragonfly

package nup

import "errors"

func renameNoReplace(string, string) error { return errors.ErrUnsupported }
