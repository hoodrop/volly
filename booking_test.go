package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseOffset(t *testing.T) {
	cases := map[string]int{
		"+8":     8 * 3600,
		"+08":    8 * 3600,
		"+0800":  8 * 3600,
		"+08:00": 8 * 3600,
		"-05:30": -(5*3600 + 30*60),
		"+00:00": 0,
	}
	for in, want := range cases {
		got, err := parseOffset(in)
		if err != nil {
			t.Errorf("parseOffset(%q): unexpected error: %v", in, err)
		} else if got != want {
			t.Errorf("parseOffset(%q) = %d, want %d", in, got, want)
		}
	}

	for _, in := range []string{"", "8", "+", "+ab:cd", "+15:00", "+08:99", "+080"} {
		if _, err := parseOffset(in); err == nil {
			t.Errorf("parseOffset(%q): expected error, got none", in)
		}
	}
}

func TestConfigLocation(t *testing.T) {
	loc, err := config{Timezone: "Asia/Singapore"}.location()
	if err != nil {
		t.Fatalf("IANA name: %v", err)
	}
	if loc.String() != "Asia/Singapore" {
		t.Errorf("got %q", loc)
	}

	loc, err = config{Timezone: "+08:00"}.location()
	if err != nil {
		t.Fatalf("fixed offset: %v", err)
	}
	if _, offset := time.Now().In(loc).Zone(); offset != 8*3600 {
		t.Errorf("fixed offset = %d, want %d", offset, 8*3600)
	}

	if _, err := (config{Timezone: "Mars/Olympus_Mons"}).location(); err == nil {
		t.Error("expected error for unknown timezone")
	}
}

func TestParseProxymanRaw(t *testing.T) {
	raw := "POST /api/book HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: 999\r\n" + // must be stripped
		"X-Multi: a\r\n" +
		"X-Multi: b\r\n" +
		"\r\n" +
		"{\"court\":1}\r\n"

	req, err := parseProxymanRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	if req.method != "POST" || req.url != "https://example.com/api/book" {
		t.Errorf("got %s %s", req.method, req.url)
	}
	if req.body != `{"court":1}` {
		t.Errorf("body = %q", req.body)
	}
	if got := req.header.Values("X-Multi"); len(got) != 2 {
		t.Errorf("X-Multi values = %v", got)
	}
	if got := req.header.Values("Content-Length"); len(got) != 0 {
		t.Errorf("Content-Length should be stripped, got %v", got)
	}

	if _, err := parseProxymanRaw("POST /x HTTP/1.1\n\n"); err == nil {
		t.Error("expected error when Host header is missing")
	}
}

func TestLoadRequestAutoAndNamed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "req.txt")
	raw := "GET /ping HTTP/1.1\nHost: example.com\n\n"
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	req, name, err := loadRequest(path, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if name != "proxyman-raw" || req.url != "https://example.com/ping" {
		t.Errorf("auto: got (%s) %s", name, req.url)
	}

	if _, _, err := loadRequest(path, "chrome-curl"); err == nil {
		t.Error("expected error for unregistered format")
	}

	if _, _, err := loadRequest(path, "auto"); err != nil {
		t.Fatal(err)
	}

	bad := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(bad, []byte("this is not an HTTP request\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadRequest(bad, "auto"); err == nil {
		t.Error("expected error when no parser accepts the file")
	}
}

func TestLoggerWritesTimestampedFile(t *testing.T) {
	dir := t.TempDir()
	loc := time.FixedZone("+08:00", 8*3600)

	log, err := newLogger(dir, loc)
	if err != nil {
		t.Fatal(err)
	}
	log.logf("hello %s", "world")
	log.close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 log file, got %d", len(entries))
	}
	if name := entries[0].Name(); !strings.HasPrefix(name, "booking-") || !strings.HasSuffix(name, ".log") {
		t.Errorf("unexpected log filename %q", name)
	}

	data, err := os.ReadFile(log.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello world") {
		t.Errorf("log file missing record, got %q", data)
	}
}
