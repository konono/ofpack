//go:build !unix

package ofpack

import "errors"

func mkfifo(string) error { return errors.New("fifos are not supported on this platform") }
