# Cortex-A53 TTL and boot log release

The current memory-optimized successor is [ttlboot3](MEMORY_OPTIMIZATION.md).
It retains the TTL and log behavior described below.

Package `0.4.3+ttlboot2-1` builds on `0.4.3+tcpfix1-1`. It retains the bounded
TCP lifecycle, original-destination lookup repair and existing CLI/LuCI.
Only the `aarch64_cortex-a53` IPK is published.

## WAN TTL

`ttl_enabled=1` installs a firewall4 postrouting rule on the WAN's current
L3 device. IPv4 packets leave with `ttl_value` (default 64), including TCP,
UDP, packets bypassing the HTTP proxy, and router-originated traffic.
IPv6 hop-limit rewriting is optional (`ttl_ipv6=1`, default off).
`ttl_device` can override automatic WAN detection for a specific device.

TTL is a kernel packet rewrite, not a change to the TCP proxy implementation.
REDIRECT remains the interception mode. It does not normalize all TCP/TLS
fingerprints or guarantee acceptance by any campus network.

Flowtable and hardware acceleration can bypass this hook on subsequent packets.
Enabling TTL therefore saves the original software/hardware flow-offload UCI
settings locally and disables them. Stopping UA-Mask or disabling TTL restores
the saved settings. This can reduce maximum routing throughput. The saved
settings contain no addresses or account details and are not part of the IPK.

This release also saves/disables the per-device `rx-gro-list` feature using
`ethtool`. On the tested kernel, UDP GRO frag-list packets left some datagrams
with their original TTL after segmentation, despite postrouting (and an
experimental netdev egress rule). Turning off `rx-gro-list` removed those
observed misses; ordinary GRO/checksum/TSO features are left unchanged.
NIC settings are restored when TTL is stopped. A net hotplug handler reapplies
the compatibility setting to recreated interfaces. Per-boot NIC state is
stored separately because NIC feature state itself resets across a reboot.

The first TTL candidate (`ttlboot1`) did not include this compatibility fix;
use `ttlboot2` for the actual deployment. The package depends on `ethtool`.

## Persistent history: total 5 MiB

`persistent_logs=1` (default on) runs an independent, lightweight native
`logread` writer and a once-per-minute shell resource sampler. Logs continue
when the HTTP proxy is stopped. The system's normal RAM syslog is unchanged.
UA-Mask writes to procd stdout/syslog while persistence is enabled, avoiding
a duplicate application log on RAM-backed `/tmp`.

History is root-readable only under `/overlay/UAmask-logs/boots/BOOT-ID/`:

- `system.log`, `system.log.old`: native syslog, kernel and plugin messages;
  each rotates at 512 KiB, retaining one previous chunk in the same boot.
- `resources.log`, `resources.log.old`: each about 64 KiB; memory, sockets,
  conntrack, actual plugin FD counts/RSS, and DNS process presence.
- `boot-info.txt`, `kernel-at-start.log` (at most 64 KiB), optional stop marker.

Each kernel boot ID gets a separate directory. Restarting a service during the
same boot does not create another boot. A hard power cut has no reliable final
shutdown message; the next boot's ID provides the boundary. Early timestamps
may precede network time synchronization, so uptime is also recorded.

The total budget is 5120 KiB (5 MiB). Older boot directories are removed first.
Pruning reserves 1536 KiB for the active boot's bounded files, filesystem blocks
and message/rotation overhead; closed histories use the remaining budget.
Thus logs are not retained forever: they survive shutdown/reboot until aged
out by the storage budget. Native logread fsyncs messages and samples are synced,
but abrupt loss of power can still lose the last uncommitted data.

The log service is enabled separately as `uamask-log-history`. Stop/disable it
explicitly to disable persistence. Its data is intentionally retained after
package removal. No diagnostics, node lists or credentials are uploaded.

## Defaults and compatibility

Fresh installs use a conservative custom profile: 128 workers, 4096-byte HTTP
buffers, a 1000-entry UA cache, GOGC 100, WARN logging. TCP ports 22/443 bypass
UA interception; TTL still applies. Fresh installs use Android with partial
regex replacement and exclude Windows. Private/campus destination ranges
bypass UA interception so local authentication and LAN traffic remain direct.
Normal upgrades preserve the existing `/etc/config/UAmask` conffile, so existing
UA and performance settings are not replaced by fresh-install defaults.

The package does not automatically authenticate campus accounts, change WAN
identity or Wi-Fi, install UA3F, or add personal VPN configurations.

Validation includes Go race/lifecycle tests, shell/Lua syntax, TTL input
validation, offload save/restore, same-boot restarts, simulated boot changes and
oldest-boot pruning under the 5 MiB limit. A53 emulation and the actual IPK
installation are validated separately in the release workflow/deployment.
