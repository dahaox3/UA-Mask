# Cortex-A53 TCP lifecycle repair

This fork release is based on upstream v0.4.3 and retains its CLI, LuCI,
firewall integration, and User-Agent rewrite policy. The package version is
`0.4.3+tcpfix1-1`, which upgrades `0.4.3-1` through normal `opkg install`.

## What changed

- Read `SO_ORIGINAL_DST` through `SyscallConn.Control`, without duplicating the
  TCP descriptor through `File()` or changing the socket's blocking behavior.
- Bound initial protocol detection and each HTTP request header read. A client
  that sends no data or leaves headers incomplete cannot hold a worker forever.
- Reclaim a connection when **both directions have no data** for the idle
  timeout. A continuously active download or upload has no fixed lifetime cap.
- After one direction finishes, drain the remaining direction while data keeps
  arriving. Close both sockets after a bounded silent half-close interval.
- Propagate forwarding errors and cancellation to both directions, wait for
  both copy loops to exit, and reset pooled buffers before returning them.
- Expire sockets waiting in the worker queue. A full queue immediately rejects
  new connections instead of blocking the accept loop and retaining a large
  kernel TCP backlog.

The observed router had about 3,600 allocated TCP sockets, hundreds of
`CLOSE_WAIT` sockets, and TCP memory-pressure warnings. Restarting UA-Mask
released most of those sockets. These observations motivated the bounded
lifecycle changes; they do not establish that every outstanding connection
had the same cause.

## Settings

All four values are positive seconds in `/etc/config/UAmask` and appear in the
LuCI advanced tab. Existing configurations use these defaults when the options
are absent.

| UCI option | CLI flag | Default |
| --- | --- | ---: |
| `first_packet_timeout` | `-first-packet-timeout` | 10 |
| `idle_timeout` | `-idle-timeout` | 600 |
| `half_close_timeout` | `-half-close-timeout` | 30 |
| `queue_timeout` | `-queue-timeout` | 5 |

The first value also bounds each HTTP header read, but does not place a fixed
deadline on request-body streaming. Idle and half-close timeouts reset when
either direction transfers data. These are **not** the old five-minute
maximum connection lifetime removed by upstream v0.4.3.

The package declares `/etc/config/UAmask` as a conffile. Normal upgrades keep
existing User-Agent, regular expressions, bypass lists, and other settings.
The repair does not add `Windows` to an existing regular expression. A regression
test verifies that a configuration excluding Windows continues to exclude it.

## Target and installation

Only `aarch64_cortex-a53` is published: Linux AArch64, ARMv8.0 baseline,
`CGO_ENABLED=0`, statically linked. It targets OpenWrt/ImmortalWrt with
`opkg`, firewall4/nftables and `luci-compat`, including the H3C NX30 Pro
used during diagnosis. It is not an APK package for newer apk-based systems.

Back up the current configuration before installing. Download the IPK and
`SHA256SUMS` from this fork's Release, check the checksum, then run:

```sh
opkg install /tmp/UAmask_0.4.3+tcpfix1-1_aarch64_cortex-a53.ipk
/etc/init.d/UAmask restart
/usr/bin/UAmask -v
```

Use the existing enable setting and UA policy. No personal VPN endpoints or
authentication details are included in the package.

## Validation and reproducible build

The regression suite covers silent/partial first packets, incomplete and
malformed HTTP headers, non-HTTP byte preservation, one-way activity,
half-close response draining, idle cleanup, cancellation, queue expiry and
overload, UA regex compatibility, and Linux nonblocking socket/deadline
preservation. The workflow runs Linux race tests repeatedly, checks shell/Lua
syntax, and executes the ARM64 binary and tests with Cortex-A53 emulation.

```sh
go test -race -count=10 -timeout=180s ./src
python3 scripts/build_ipk.py
```

The build uses Go 1.27.3 in CI, strips host paths and build IDs, and fixes tar/gzip
timestamps to the source commit timestamp. `BUILDINFO.json` records the exact
commit, architecture and SHA-256 hashes. The IPK uses the same gzip/tar outer
format and standard OpenWrt package hooks as the upstream A53 package.

Building and publishing this Release does not install it on any router.
