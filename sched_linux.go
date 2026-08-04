//go:build linux

package main

import (
	"os"
	"runtime"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
)

// applyScheduling is the in-process equivalent of launching the binary via
//
//	chrt -f <rt_priority> taskset -c <cpu_core> ./booking
//
// Both settings are best-effort: without CAP_SYS_NICE the kernel denies
// SCHED_FIFO, and the requested core may not exist — either way we log a
// warning and run on with normal scheduling rather than failing the launch.
func applyScheduling(cfg config, logf func(string, ...any)) {
	if cfg.CPUCore >= 0 {
		pinThreads(cfg.CPUCore, logf)
	}
	if cfg.RTPriority > 0 {
		makeRealtime(cfg.RTPriority, logf)
	}
}

// threadIDs lists every thread of this process. Scheduling policy and CPU
// affinity are per-thread attributes on Linux: `chrt … ./booking` sets them
// before exec so every future thread inherits, whereas we must stamp each
// already-running thread ourselves. Threads spawned afterwards inherit from
// their (by then stamped) creator via clone(2).
func threadIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, err
	}
	tids := make([]int, 0, len(entries))
	for _, e := range entries {
		if tid, err := strconv.Atoi(e.Name()); err == nil {
			tids = append(tids, tid)
		}
	}
	return tids, nil
}

// pinThreads sets the CPU affinity of every thread to core alone.
func pinThreads(core int, logf func(string, ...any)) {
	if n := runtime.NumCPU(); core >= n {
		logf("sched: cpu_core %d out of range (machine has %d cores) — running unpinned", core, n)
		return
	}
	var set unix.CPUSet
	set.Set(core)

	tids, err := threadIDs()
	if err != nil {
		logf("sched: cannot list threads: %v — running unpinned", err)
		return
	}
	failed := 0
	for _, tid := range tids {
		if err := unix.SchedSetaffinity(tid, &set); err != nil {
			failed++
		}
	}
	switch {
	case failed == len(tids):
		logf("sched: pinning to core %d failed for all %d threads — running unpinned", core, len(tids))
	case failed > 0:
		logf("sched: pinned %d/%d threads to core %d", len(tids)-failed, len(tids), core)
	default:
		logf("sched: pinned all %d threads to core %d", len(tids), core)
	}
}

// schedParam mirrors struct sched_param from <linux/sched.h>.
type schedParam struct {
	SchedPriority int32
}

// schedSetSchedulerFIFO is sched_setscheduler(2) with policy SCHED_FIFO.
// x/sys/unix ships no wrapper for this one, so invoke the syscall directly.
func schedSetSchedulerFIFO(tid, prio int) error {
	param := schedParam{SchedPriority: int32(prio)}
	_, _, errno := unix.Syscall(unix.SYS_SCHED_SETSCHEDULER,
		uintptr(tid), uintptr(unix.SCHED_FIFO),
		uintptr(unsafe.Pointer(&param)))
	if errno != 0 {
		return errno
	}
	return nil
}

// makeRealtime switches every thread to SCHED_FIFO at the given priority, so
// a spinning goroutine can't be preempted inside its final busy-wait window.
func makeRealtime(prio int, logf func(string, ...any)) {
	tids, err := threadIDs()
	if err != nil {
		logf("sched: cannot list threads: %v — rt_priority ignored", err)
		return
	}
	failed, permDenied := 0, false
	for _, tid := range tids {
		if err := schedSetSchedulerFIFO(tid, prio); err != nil {
			failed++
			permDenied = permDenied || err == unix.EPERM
		}
	}
	hint := ""
	if permDenied {
		hint = " — run as root, or once: sudo setcap 'cap_sys_nice+ep' ./booking (re-apply after every rebuild)"
	}
	switch {
	case failed == len(tids):
		logf("sched: SCHED_FIFO %d denied for all %d threads%s — continuing with normal scheduling", prio, len(tids), hint)
	case failed > 0:
		logf("sched: SCHED_FIFO %d set on %d/%d threads%s", prio, len(tids)-failed, len(tids), hint)
	default:
		logf("sched: SCHED_FIFO %d set on all %d threads", prio, len(tids))
	}
}
