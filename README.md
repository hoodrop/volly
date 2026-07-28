# Booking Sniper [![CI](https://github.com/hoodrop/booking-sniper/actions/workflows/ci.yml/badge.svg)](https://github.com/hoodrop/booking-sniper/actions/workflows/ci.yml)

A precision request sniper for online bookings. Paste the exact booking HTTP
request, tell it when the booking window opens, and it fires a burst of
requests with sub-millisecond launch accuracy — the first `200 OK` wins and
instantly cancels everything still in flight or pending.

## How it works

1. **Parse** — reads a raw HTTP request pasted from Proxyman's RAW view or
   curl command out of `request.txt`.
2. **Schedule** — you give it a launch time (or fire immediately), a request
   count, and an interval. Request *i* is due at `start + (i-1) * interval` —
   absolute-time scheduling, so one late wake-up never pushes the rest later.
3. **Fire** — each request gets its own goroutine that sleeps until ~2ms
   before its deadline, then busy-spins against the wall clock. Launches land
   within tens of microseconds of target.
4. **Win** — the first `200` sets an atomic flag and cancels the shared
   context: in-flight requests abort, pending launches skip. Every record
   (target time, actual fire time, status, latency, body) goes to stdout and a
   fresh timestamped log file under `logs/`.

## Requirements

- Go 1.26+ (no third-party dependencies)

## Setup

1. **Capture the request.** In your browser or Proxyman, perform the booking
   step once and copy the request as raw HTTP. Paste it into `request.txt`:

   ```
   POST /foo/bar/ HTTP/1.1
   Host: example.com
   Authorization: Bearer ...
   Content-Type: application/json

   {"foo": "bar", ...}
   ```

   The parser rebuilds the URL from the `Host` header (https is assumed) and
   strips `Content-Length` so a stale copied value can't break the request.

2. **Run once** to generate `config.json` with defaults, then edit if needed.

## Usage

```sh
go run .          # or: go build -o booking . && ./booking
```
