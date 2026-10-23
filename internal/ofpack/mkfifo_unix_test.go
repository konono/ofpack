//go:build unix

package ofpack

import "syscall"

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o644) }
