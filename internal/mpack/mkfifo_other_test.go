//go:build !unix

package mpack

import "errors"

func mkfifo(string) error { return errors.New("fifos are not supported on this platform") }
