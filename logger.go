package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// logger is the single writer for all log records: every queued line goes to
// both stdout and the buffered log file. Request goroutines never touch the
// file directly and never block on disk I/O beyond a channel send; the file
// is flushed and closed once, at close.
type logger struct {
	loc  *time.Location
	file *os.File
	path string // path of this run's log file, e.g. logs/volly-2026-07-26_22-30-05.log
	ch   chan string
	done chan struct{}
}

// newLogger creates dir if needed, opens a fresh log file for this run —
// named with the run start time in loc — and launches the writer goroutine.
func newLogger(dir string, loc *time.Location) (*logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating log dir %s: %w", dir, err)
	}
	name := "volly-" + time.Now().In(loc).Format("2006-01-02_15-04-05") + ".log"
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating log file %s: %w", path, err)
	}

	l := &logger{
		loc:  loc,
		file: f,
		path: path,
		ch:   make(chan string, 4096),
		done: make(chan struct{}),
	}
	go func() {
		w := bufio.NewWriterSize(f, 64*1024)
		for line := range l.ch {
			fmt.Println(line)
			_, _ = w.WriteString(line + "\n")
		}
		_ = w.Flush()
		close(l.done)
	}()
	return l, nil
}

// stamp renders t in the configured timezone with microsecond precision.
// Every log record carries one, stamped when the event happened — not when
// the logger goroutine got around to writing it.
func (l *logger) stamp(t time.Time) string {
	return t.In(l.loc).Format("15:04:05.000000")
}

// logf stamps the record with the current time (µs precision) and queues it
// for the writer. Records that need an exact event time (target, fire) stamp
// it into the message itself, since the leading stamp is the time the record
// was created (e.g. when the response arrived).
func (l *logger) logf(format string, args ...any) {
	l.ch <- l.stamp(time.Now()) + " " + fmt.Sprintf(format, args...)
}

// close flushes the file buffer, waits for the writer goroutine to exit,
// and closes the log file.
func (l *logger) close() {
	close(l.ch)
	<-l.done
	_ = l.file.Close()
}
