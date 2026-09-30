package main

import "time"

// cpuTime is not measured on Windows; a negative value leaves it out of the report.
func cpuTime() time.Duration { return -1 }

// rss is not measured on Windows; 0 leaves it out of the report.
func rss() uint64 { return 0 }
