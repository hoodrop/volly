package main

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// promptString prints a prompt, reads one line of stdin, and returns the
// trimmed input, or def if the user just pressed Enter.
func promptString(reader *bufio.Reader, prompt, def string) string {
	fmt.Printf("%s [%s]: ", prompt, def)
	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// promptInt prompts for an integer, keeping def on empty input or parse failure.
func promptInt(reader *bufio.Reader, prompt string, def int) int {
	for {
		raw := promptString(reader, prompt, strconv.Itoa(def))
		val, err := strconv.Atoi(raw)
		if err != nil {
			fmt.Printf("  not a valid integer, try again\n")
			continue
		}
		return val
	}
}

// promptDuration prompts for a duration in milliseconds, keeping def on
// empty input or parse failure.
func promptDuration(reader *bufio.Reader, prompt string, def time.Duration) time.Duration {
	defMs := strconv.FormatInt(def.Milliseconds(), 10)
	for {
		raw := promptString(reader, prompt, defMs)
		ms, err := strconv.Atoi(raw)
		if err != nil || ms < 0 {
			fmt.Printf("  not a valid non-negative integer (milliseconds), try again\n")
			continue
		}
		return time.Duration(ms) * time.Millisecond
	}
}

// zoneLabel describes loc for prompt text: the location name plus the current
// abbreviation when they differ — e.g. "Asia/Singapore (+08)", or just
// "+08:00" for a fixed offset.
func zoneLabel(loc *time.Location) string {
	label := loc.String()
	if name, _ := time.Now().In(loc).Zone(); name != label {
		label += " (" + name + ")"
	}
	return label
}

// promptLaunchTime asks for a launch time expressed in loc (as
// "YYYY-MM-DD HH:MM:SS" or just "HH:MM:SS" for today, either with an
// optional fractional second), and returns the equivalent instant in UTC
// (the machine's local clock). Empty input means "launch immediately".
func promptLaunchTime(reader *bufio.Reader, loc *time.Location) (time.Time, bool) {
	fmt.Printf("Enter launch time in %s.\n", zoneLabel(loc))
	fmt.Println("Formats: \"1970-01-01 09:00:00.500\" or just \"09:00:00.500\" for today — fraction optional, introduced by \".\", \",\" or \":\" (\"11:59:59:500000\" works too). Leave blank to launch immediately.")
	raw := promptString(reader, "Launch time", "")
	if raw == "" {
		return time.Time{}, false
	}

	parsed, err := parseLaunchTime(raw, loc)
	if err != nil {
		fmt.Printf("  couldn't parse %q, launching immediately instead\n", raw)
		return time.Time{}, false
	}

	return parsed.UTC(), true
}

// parseLaunchTime parses a launch time in loc: "HH:MM:SS" (today in loc, or
// tomorrow when that time already passed) or "YYYY-MM-DD HH:MM:SS". The
// seconds may carry a fraction — "11:59:59.500", "11:59:59:500000" — so the
// launch can be aimed at a sub-second instant.
func parseLaunchTime(raw string, loc *time.Location) (time.Time, error) {
	if date, clock, found := strings.Cut(raw, " "); found {
		return time.ParseInLocation("2006-01-02 15:04:05", date+" "+normalizeFraction(clock), loc)
	}

	// Time-only: assume today's date in loc.
	t, err := time.ParseInLocation("15:04:05", normalizeFraction(raw), loc)
	if err != nil {
		return time.Time{}, err
	}
	nowLoc := time.Now().In(loc)
	parsed := time.Date(nowLoc.Year(), nowLoc.Month(), nowLoc.Day(),
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
	if parsed.Before(nowLoc) {
		// Time already passed today — assume they mean tomorrow.
		parsed = parsed.Add(24 * time.Hour)
	}
	return parsed, nil
}

// normalizeFraction rewrites a colon-separated fractional second
// ("11:59:59:500000") into the decimal-point form Go's parser accepts
// ("11:59:59.500000"). Anything else passes through unchanged — Go already
// handles "." and "," fractions on its own.
func normalizeFraction(clock string) string {
	fields := strings.Split(clock, ":")
	if len(fields) != 4 {
		return clock
	}
	return strings.Join(fields[:3], ":") + "." + fields[3]
}
