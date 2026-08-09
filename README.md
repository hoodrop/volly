# volly &middot; [![CI](https://github.com/hoodrop/volly/actions/workflows/ci.yml/badge.svg)](https://github.com/hoodrop/volly/actions/workflows/ci.yml)

A precise HTTP request repeating launcher made by Go. Paste the exact HTTP
request, tell volly when to fire, and it sends a burst of requests with
sub-millisecond launch accuracy — the first `200 OK` wins and instantly
cancels everything still in flight or pending.

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

1. **Capture the request.** Send the request once manually, then copy it
   into `request.txt` in either supported format — with
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
continues with normal scheduling. (In the current version cpu_core pinning
is disabled by default as its performance may differ from expected.)

`SCHED_FIFO` requires `CAP_SYS_NICE`. Run as root, or grant just that one
capability to the binary (re-apply after every rebuild):

```sh
sudo setcap 'cap_sys_nice+ep' ./volly
```

Off Linux the knobs are ignored (with a note in the log). To see what this
buys on a given machine, compare the `launch jitter` line in the logs with
the feature on and off.

## Usage

The default Linux flow is three separate steps — build, grant the capability
`SCHED_FIFO` needs, then run:

```sh
go build -o volly .
sudo setcap 'cap_sys_nice+ep' ./volly  # re-apply after every rebuild
./volly
```

The `setcap` step is optional: without it (or off Linux) the program logs a
warning and runs with normal scheduling. `go run .` also works, but the
capability can't stick to its throwaway temp binary, so that path always
runs without `SCHED_FIFO`.

## Choosing the interval

Every request goroutine sleeps until `spinMargin` (2ms, see `runner.go`)
before its absolute deadline, then busy-spins against the wall clock. Since
all goroutines wait independently, an interval shorter than 2ms does not
break anything — a request whose deadline has already passed fires
immediately ("catch-up, never drop"), and any lateness shows up honestly in
the `launch jitter` log line. What changes is the cost, and in one setup
the accuracy:

- **CPU.** The number of goroutines busy-spinning at any moment is roughly
  `spinMargin / interval` — ~2 at a 1ms interval, ~4 at 0.5ms — each
  pinning a core at 100% for the whole run.
- **Accuracy.** Unpinned (the default), each spinner gets its own core and
  jitter stays low. But with `cpu_core` pinning *and* `SCHED_FIFO` both
  enabled, equal-priority FIFO threads on a single core don't timeshare:
  whichever spinner grabs the core holds it until it fires, so a goroutine
  with an earlier deadline can miss its target entirely and only fire late.
  With very small intervals the first few launches also jitter more, since
  their deadlines may pass while the goroutine spawn loop is still ramping
  up.

Practical guidance: keep the interval at 5ms or above (the config's own
recommendation). If you genuinely need sub-2ms spacing, shrink `spinMargin`
(e.g. to 200µs) — the spinning crowd shrinks proportionally, at the cost of
the sleep phase's wake-up error (tens of µs up to ~1ms) landing directly in
the jitter instead of being absorbed by the spin.

## gRPC API (serve mode)

Besides the interactive CLI, volly can run as a gRPC server so another
program can trigger bursts directly — no `request.txt` involved; the caller
passes method, URL, headers and body in the RPC itself:

```sh
./volly serve    # binds config.json's grpc_listen, default 127.0.0.1:50051
```

The service is defined in `proto/launcher/v1/launcher.proto`; generated Go
code lives in `gen/` and is committed, so building never needs protoc. One
RPC, unary: `Launch(LaunchRequest) returns (LaunchResponse)` — it blocks
until the burst ends (first 200, or everything fired) and returns whether it
won, the winning status/body, fired/aborted/skipped counts, jitter stats,
and the run's log file path (per-request details go to the log, same as CLI
runs). Cancelling the RPC cancels the run. Only one run at a time: a second
`Launch` while one is in progress fails with `FAILED_PRECONDITION`.

Go client sketch:

```go
conn, _ := grpc.NewClient("127.0.0.1:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
client := launcherv1.NewLauncherClient(conn)
resp, err := client.Launch(ctx, &launcherv1.LaunchRequest{
    Method:   "POST",
    Url:      "https://example.com/foo/bar/",
    Headers:  []*launcherv1.Header{{Name: "Authorization", Value: "Bearer ..."}},
    Body:     `{"foo": "bar"}`,
    LaunchAt: timestamppb.New(time.Date(2026, 8, 11, 9, 0, 0, 0, time.Local)), // omit to fire now
    NumCalls: 500,
    Interval: durationpb.New(30 * time.Millisecond),
})
```

The API is plaintext gRPC bound to localhost by default — expose it beyond
localhost only deliberately (e.g. behind mTLS). To regenerate the stubs
after editing the proto: `brew install buf`,
`go install google.golang.org/protobuf/cmd/protoc-gen-go@latest google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest`,
then `buf generate`.
