//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cpuTime is the user and system CPU time used by this process so far.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// rss is the resident memory of this process in bytes, SQLite's included
// (its allocations do not appear in Go's memory statistics), or 0.
func rss() uint64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	kb, _ := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	return kb * 1024
}
