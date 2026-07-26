package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

// configFile is read once at startup. If it doesn't exist it is created with
// the defaults below, so the available knobs are self-documenting.
const configFile = "config.json"

// config holds every user-tunable setting. num_calls and interval_ms are the
// *defaults* offered by the interactive prompts (typing a different value at
// the prompt overrides them for that run); the rest are only configurable
// here.
type config struct {
	NumCalls      int    `json:"num_calls"`      // default total request amount
	IntervalMs    int    `json:"interval_ms"`    // default interval between launches, milliseconds
	Timezone      string `json:"timezone"`       // IANA name ("Asia/Singapore") or fixed offset ("+08:00")
	RequestFormat string `json:"request_format"` // parser name from requestParsers, or "auto"
	RequestFile   string `json:"request_file"`   // file containing the pasted request
	LogDir        string `json:"log_dir"`        // each run writes a fresh timestamped file here
}

func defaultConfig() config {
	return config{
		NumCalls:      700,
		IntervalMs:    30,
		Timezone:      "Asia/Singapore",
		RequestFormat: "auto",
		RequestFile:   "request.txt",
		LogDir:        "logs",
	}
}

// loadConfig reads path into a config. A missing file is not an error: it is
// created with the defaults and the defaults are returned.
func loadConfig(path string) (config, error) {
	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		data, _ = json.MarshalIndent(cfg, "", "  ")
		if werr := os.WriteFile(path, append(data, '\n'), 0o644); werr != nil {
			return cfg, fmt.Errorf("creating default %s: %w", path, werr)
		}
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

func (c config) validate() error {
	switch {
	case c.NumCalls <= 0:
		return fmt.Errorf("num_calls must be > 0, got %d", c.NumCalls)
	case c.IntervalMs < 0:
		return fmt.Errorf("interval_ms must be >= 0, got %d", c.IntervalMs)
	case c.RequestFile == "":
		return fmt.Errorf("request_file must not be empty")
	case c.LogDir == "":
		return fmt.Errorf("log_dir must not be empty")
	}
	return nil
}

// location resolves the configured timezone: first as an IANA name (uses the
// system tz database, handles DST), then as a fixed numeric offset like
// "+08:00", "+0800" or "+8" — useful on machines without tzdata installed.
func (c config) location() (*time.Location, error) {
	if loc, err := time.LoadLocation(c.Timezone); err == nil {
		return loc, nil
	}
	offset, err := parseOffset(c.Timezone)
	if err != nil {
		return nil, fmt.Errorf("timezone %q is neither an IANA name nor a numeric offset like \"+08:00\"", c.Timezone)
	}
	return time.FixedZone(c.Timezone, offset), nil
}

// parseOffset parses a numeric UTC offset ("+8", "+08", "+0800", "+08:00",
// and negative forms) into seconds east of UTC.
func parseOffset(s string) (int, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid offset %q", s)
	}
	var sign int
	switch s[0] {
	case '+':
		sign = 1
	case '-':
		sign = -1
	default:
		return 0, fmt.Errorf("offset %q must start with + or -", s)
	}
	rest := s[1:]

	var hours, minutes string
	switch {
	case strings.Contains(rest, ":"):
		parts := strings.SplitN(rest, ":", 2)
		hours, minutes = parts[0], parts[1]
	case len(rest) == 4:
		hours, minutes = rest[:2], rest[2:]
	case len(rest) <= 2:
		hours = rest
	default:
		return 0, fmt.Errorf("invalid offset %q", s)
	}

	h, err := strconv.Atoi(hours)
	if err != nil || h > 14 {
		return 0, fmt.Errorf("invalid offset hours in %q", s)
	}
	m := 0
	if minutes != "" {
		m, err = strconv.Atoi(minutes)
		if err != nil || m > 59 {
			return 0, fmt.Errorf("invalid offset minutes in %q", s)
		}
	}
	return sign * (h*3600 + m*60), nil
}
