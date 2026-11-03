package main

import (
	"os"
	"syscall"
)

// redirectStdio points this process's stdout and stderr at f, so output
// written straight to them (a crash report) follows the log to its
// current file.
func redirectStdio(f *os.File) {
	fd := int(f.Fd())
	syscall.Dup3(fd, 1, 0)
	syscall.Dup3(fd, 2, 0)
}
