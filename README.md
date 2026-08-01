# Booking Sniper &middot; [![CI](https://github.com/hoodrop/booking-sniper/actions/workflows/ci.yml/badge.svg)](https://github.com/hoodrop/booking-sniper/actions/workflows/ci.yml)

A precision request sniper for online bookings. Paste the exact booking HTTP
request, tell it when the booking window opens, and it fires a burst of
requests with sub-millisecond launch accuracy — the first `200 OK` wins and
instantly cancels everything still in flight or pending.

## How it works

1. **Parse** — reads the request pasted into `request.txt`, auto-detecting
   the format: raw HTTP from Proxyman's RAW view, or a curl command from
   browser devtools' "Copy as cURL".
2. **Schedule** — you give it a launch time (or fire immediately), a request
   count, and an interval. Request *i* is due at `start + (i-1) * interval` —
   absolute-time scheduling, so one late wake-up never pushes the rest later.
3. **Fire** — each request gets its own goroutine that sleeps until ~2ms
   before its deadline, then busy-spins against the wall clock. Launches land
   within tens of microseconds of target. On Linux the process additionally
   pins itself to one core and switches to `SCHED_FIFO` (see below).
4. **Win** — the first `200` sets an atomic flag and cancels the shared
   context: in-flight requests abort, pending launches skip. Every record
   (target time, actual fire time, status, latency, body) goes to stdout and a
   fresh timestamped log file under `logs/`.

## Requirements

- Go 1.26+
- One third-party module: `golang.org/x/sys`, used for the Linux scheduling
  syscalls. Real-time scheduling itself needs Linux + `CAP_SYS_NICE` — see
  below.

## Setup

1. **Capture the request.** Perform the booking step once, then copy the
   exact request into `request.txt` in either supported format — with
   `request_format: "auto"` the right parser is picked automatically.

   **Raw HTTP** (Proxyman's RAW view):

   ```
   POST /foo/bar/ HTTP/1.1
   Host: example.com
   Authorization: Bearer ...
   Content-Type: application/json

   {"foo": "bar", ...}
   ```

   **curl** (browser devtools → right-click the request → Copy → Copy as
   cURL):

   ```sh
   curl 'https://example.com/foo/bar/' \
     -H 'Authorization: Bearer ...' \
     -H 'Content-Type: application/json' \
     --data-raw '{"foo": "bar", ...}'
   ```

   The raw parser rebuilds the URL from the `Host` header (https is
   assumed); the curl parser understands the flag subset devtools actually
   emits (`-H`, `-X`, `--url`, `-b`, `--data*`). Both strip `Content-Length`
   so a stale copied value can't break the request.

2. **Run once** to generate `config.json` with defaults, then edit if needed.

## Low-latency scheduling (Linux)

At startup the program applies the equivalent of
`chrt -f <rt_priority> taskset -c <cpu_core>` to itself: every thread is
pinned to `cpu_core` and switched to `SCHED_FIFO` at `rt_priority`, so a
goroutine can't be migrated off its core or preempted inside its final ~2ms
busy-spin. Both are `config.json` knobs (`rt_priority: 0` / `cpu_core: -1`
disables) and both are best-effort — a failure logs a warning and the run
continues with normal scheduling.

`SCHED_FIFO` requires `CAP_SYS_NICE`. Run as root, or grant just that one
capability to the binary (re-apply after every rebuild):

```sh
sudo setcap 'cap_sys_nice+ep' ./booking
```

Off Linux the knobs are ignored (with a note in the log). To see what this
buys on a given machine, compare the `launch jitter` line in the logs with
the feature on and off.

## Usage

```sh
go run .          # or: go build -o booking . && ./booking
```
