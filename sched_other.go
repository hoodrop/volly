//go:build !linux

package main

// applyScheduling is a no-op off Linux: sched_setscheduler/sched_setaffinity
// (the syscalls behind chrt/taskset) don't exist there. Warn once if the
// knobs are enabled so the config doesn't silently do nothing.
func applyScheduling(cfg config, logf func(string, ...any)) {
	if cfg.RTPriority > 0 || cfg.CPUCore >= 0 {
		logf("sched: rt_priority/cpu_core are Linux-only — ignored on this platform")
	}
}
