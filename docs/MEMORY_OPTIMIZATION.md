# Cortex-A53 memory release: 0.4.3+ttlboot3-1

This release reduces unused and per-connection allocations while retaining the
TTL/GRO compatibility fix, 5 MiB boot history and bounded TCP lifecycle.

## Changes

- Do not construct the dynamic firewall manager when neither non-HTTP bypass
  nor a firewall UA whitelist is configured. This avoids three 10,000-entry
  channels (1,120,000 bytes of backing elements on 64-bit targets), its worker
  and timers. Enabled dynamic-firewall behavior is unchanged; its queue/map
  redesign is not part of this release.
- Server-to-client forwarding uses a pooled 8 KiB buffer instead of a fresh
  32 KiB buffer. Both directions still pass through activity tracking; idle
  and half-close semantics remain intact. Client-to-server raw forwarding
  continues to reuse the bufio reader. HTTP writers are acquired only when
  an HTTP request has been parsed successfully.
- Bound HTTP request-line plus headers to 64 KiB by default, with at most 256
  parsed fields including Host. Account for prefetched bytes, preserve the
  same buffered stream across requests, and remove the byte limit before
  streaming bodies. Chunked trailers retain net/http's buffer-size bound.
  Oversized/malformed requests close the connection; bodies are not drained
  after a forwarding error, which could otherwise delay socket cleanup.
- Bound the UA LRU by both entry count and charged bytes. The default charge
  budget is 512 KiB, including the lengths of both strings and a 128-byte
  allowance per entry. This is an accounting budget, not an RSS ceiling.
  Strings are cloned on admission to avoid retaining larger backing strings.
  Inputs or replacements over 2 KiB are processed but not cached.
  `cache-size=0` or `cache-max-bytes=0` now safely disables caching.
- Extend the existing statistics file and persistent resource samples with
  heap allocation, runtime memory usage, GC count, goroutine count, charged
  cache size, and queued/rejected connection counters. No debug HTTP service
  is added. Precompile firewall set-name validation.

## Configuration and upgrade

Existing UCI settings and the configured UA policy are preserved. New options
fall back to defaults when absent from an upgraded conffile:

| UCI option | CLI flag | Default |
|---|---|---|
| `cache_max_bytes` | `-cache-max-bytes` | 524288 |
| `max_header_bytes` | `-max-header-bytes` | 65536 |

Cache budgets accept 0 through 16 MiB; header limits accept 1 KiB through 1 MiB.
The LuCI descriptions distinguish HTTP buffers from the 8 KiB return buffer.
128 active workers and GOGC 100 remain the fresh-install defaults. TTL and NIC
offload behavior are inherited from ttlboot2; no forwarding-path shortcut is
introduced. The log budget, per-boot boundaries and sync behavior are preserved.

## Validation and limits

Regression tests cover byte limits at the boundary, fragmented/prefetched
headers, oversized request lines/field counts, pipelined requests, streamed
bodies larger than the header budget, chunked bodies/trailers, nil/disabled
caches, cache eviction and concurrent accounting, lazy HTTP buffer allocation,
UA policy and the existing timeout/half-close/shutdown tests.

`BenchmarkRelayLoopback` compares the old default copy buffer with the new
pooled buffer using a short, actual TCP loopback transfer on the target CPU.
It does not measure WAN or Wi-Fi routing capacity. Allocations/RSS depend on
concurrency, traffic and GC state; object-size savings are not an equal-sized
RSS guarantee. There is no hard process RSS limit or claim of indefinite
stability. Only the A53 64-bit IPK is published.
