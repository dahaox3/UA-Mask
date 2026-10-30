#!/bin/sh
# Bounded persistent syslog + resource history, grouped by kernel boot ID.
set -eu
BASE=${UAMASK_HISTORY_DIR:-/overlay/UAmask-logs}
BOOT_ID_FILE=${UAMASK_BOOT_ID_FILE:-/proc/sys/kernel/random/boot_id}
BUDGET_KIB=5120
# logread: two <=512 KiB files (+ a bounded syslog record); resource files:
# two <=64 KiB files (+ one sample); kernel snapshot <=64 KiB. Reserve 1536.
ACTIVE_RESERVE_KIB=1536
umask 077

boot_id() {
    read -r id <"$BOOT_ID_FILE"
    [ "${#id}" = 36 ] || return 1
    case "$id" in *[!0-9a-f-]*) return 1;; esac
    printf '%s\n' "$id"
}

prune() {
    id=$(boot_id)
    current="$BASE/boots/$id"
    [ -d "$current" ] || return 1
    while :; do
        total=$(du -sk "$BASE" | awk '{print $1}')
        active=$(du -sk "$current" | awk '{print $1}')
        [ "$((total - active))" -gt "$((BUDGET_KIB - ACTIVE_RESERVE_KIB))" ] || break
        oldest=$(awk -v current="$id" '$0 != current {print; exit}' "$BASE/boots.index")
        [ -n "$oldest" ] || break
        [ "${#oldest}" = 36 ] || return 1
        case "$oldest" in *[!0-9a-f-]*) return 1;; esac
        # Fixed prefix plus validated kernel UUID; never traverse outside BASE.
        rm -rf "$BASE/boots/$oldest"
        awk -v old="$oldest" '$0 != old' "$BASE/boots.index" >"$BASE/boots.index.new"
        mv -f "$BASE/boots.index.new" "$BASE/boots.index"
    done
}

prepare() {
    id=$(boot_id)
    current="$BASE/boots/$id"
    mkdir -p "$current"
    chmod 700 "$BASE" "$BASE/boots" "$current"
    touch "$BASE/boots.index"
    if [ ! -f "$current/boot-info.txt" ]; then
        { date -Iseconds; printf 'boot_id=%s\n' "$id"; cat /proc/uptime
          cat /etc/openwrt_release 2>/dev/null || true
        } >"$current/boot-info.txt"
        dmesg 2>/dev/null | tail -c 65536 >"$current/kernel-at-start.log" || true
    fi
    # Recover directories left between mkdir and index update by a power cut.
    # Put unindexed old boots first; keep current last, without duplicates.
    : >"$BASE/boots.index.new"
    for directory in "$BASE"/boots/*; do
        [ -d "$directory" ] || continue
        other=${directory##*/}
        [ "$other" != "$id" ] || continue
        [ "${#other}" = 36 ] || continue
        case "$other" in *[!0-9a-f-]*) continue;; esac
        grep -Fqx "$other" "$BASE/boots.index" || printf '%s\n' "$other" >>"$BASE/boots.index.new"
    done
    awk -v current="$id" '$0 != current && !seen[$0]++' "$BASE/boots.index" >>"$BASE/boots.index.new"
    printf '%s\n' "$id" >>"$BASE/boots.index.new"
    mv -f "$BASE/boots.index.new" "$BASE/boots.index"
    # Same-boot service restarts append to the same history, not a new boot.
    printf '%s\n' "$id" >"$BASE/current-boot"
    prune
    sync
    printf '%s\n' "$current"
}

sample() {
    id=$(boot_id)
    current="$BASE/boots/$id"
    [ -d "$current" ] || return 1
    file="$current/resources.log"
    if [ -f "$file" ] && [ "$(wc -c <"$file")" -ge 65536 ]; then
        mv -f "$file" "$file.old"
    fi
    {
        printf 'time=%s ' "$(date -Iseconds)"
        read -r uptime rest </proc/uptime
        printf 'uptime_s=%s ' "$uptime"
        awk '/^(MemAvailable|MemFree|Cached|AnonPages|Slab|SReclaimable|SUnreclaim):/ {gsub(":","",$1);printf "%s_kib=%s ",$1,$2}' /proc/meminfo
        awk '$1=="TCP:" || $1=="UDP:" {p=$1;gsub(":","",p);for(i=2;i<NF;i+=2)printf "%s_%s=%s ",p,$i,$(i+1)}' /proc/net/sockstat
        printf 'conntrack=%s ' "$(cat /proc/sys/net/netfilter/nf_conntrack_count 2>/dev/null || echo unknown)"
        for name in UAmask ua3f; do
            pids=$(pidof "$name" 2>/dev/null || true)
            for pid in $pids; do
                printf '%s_pid=%s ' "$name" "$pid"
                [ ! -r "/proc/$pid/status" ] || awk -v name="$name" '/^(VmRSS|Threads):/ {gsub(":","",$1);printf "%s_%s=%s ",name,$1,$2}' "/proc/$pid/status"
                printf '%s_fds=%s ' "$name" "$(ls "/proc/$pid/fd" 2>/dev/null | wc -l)"
            done
        done
        if [ -r /tmp/UAmask.stats ]; then
            awk -F: '$1 ~ /^(heap_alloc_bytes|go_memory_bytes|gc_cycles|goroutines|cache_entries|cache_charged_bytes|queued_connections|rejected_connections)$/ && $2 ~ /^[0-9]+$/ {printf "UAmask_%s=%s ",$1,$2}' /tmp/UAmask.stats
        fi
        printf 'dnsmasq_pids=%s\n' "$(pidof dnsmasq 2>/dev/null | tr ' ' ',' || true)"
    } >>"$file"
    prune
    sync
}

case "${1:-}" in
    prepare) prepare;;
    sample) sample;;
    prune) prune;;
    run) while :; do sample; sleep 60; done;;
    stop)
        id=$(boot_id)
        { date -Iseconds; cat /proc/uptime; echo 'logging-service-stop (may be shutdown or manual stop)'; } >"$BASE/boots/$id/stop-info.txt"
        sync;;
    status) cat "$BASE/current-boot"; du -sk "$BASE";;
    *) echo 'Usage: uamask-log-history {prepare|sample|prune|run|stop|status}' >&2; exit 2;;
esac
