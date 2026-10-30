#!/bin/sh
# IPv4 TTL / optional IPv6 hop-limit normalization for the fw4 A53 package.
set -eu
STATE=${UAMASK_TTL_STATE:-/etc/UAmask-ttl.offload}
RULE=${UAMASK_TTL_RULE:-/tmp/UAmask-ttl.nft}

render() {
    device=$1 value=$2 ipv6=$3
    case "$device" in ''|*[!a-zA-Z0-9_.:-]*) echo 'Invalid WAN device' >&2; return 1;; esac
    case "$value" in ''|*[!0-9]*) echo 'Invalid TTL' >&2; return 1;; esac
    [ "$value" -ge 1 ] && [ "$value" -le 255 ] || return 1
    case "$ipv6" in 0|1) ;; *) return 1;; esac
    cat <<EOF
chain UAmask_ttl_postrouting {
    type filter hook postrouting priority mangle; policy accept;
    meta nfproto ipv4 oifname "$device" counter ip ttl set $value comment "UA-Mask WAN IPv4 TTL"
EOF
    if [ "$ipv6" = 1 ]; then
        echo "    meta nfproto ipv6 oifname \"$device\" counter ip6 hoplimit set $value comment \"UA-Mask WAN IPv6 hop-limit\""
    fi
    echo '}'
}

restore() {
    [ -f "$STATE" ] || return 0
    { read -r soft; read -r hard; } <"$STATE"
    case "$soft:$hard" in *[!01_:unset]*) return 1;; esac
    for key in flow_offloading flow_offloading_hw; do
        if [ "$key" = flow_offloading ]; then value=$soft; else value=$hard; fi
        case "$value" in
            0|1) uci set "firewall.@defaults[0].$key=$value";;
            unset) uci -q delete "firewall.@defaults[0].$key" || true;;
            *) return 1;;
        esac
    done
    uci commit firewall
    rm -f "$STATE"
}

prepare() {
    enabled=$(uci -q get UAmask.main.ttl_enabled || echo 0)
    if [ "$enabled" != 1 ]; then
        rm -f "$RULE"
        restore
        return
    fi
    command -v fw4 >/dev/null 2>&1 || { echo 'TTL rewriting requires firewall4' >&2; return 1; }
    device=$(uci -q get UAmask.main.ttl_device || true)
    [ -n "$device" ] || device=$(ubus call network.interface.wan status 2>/dev/null | jsonfilter -e '@.l3_device' || true)
    [ -n "$device" ] || device=$(uci -q get network.wan.device || true)
    value=$(uci -q get UAmask.main.ttl_value || echo 64)
    ipv6=$(uci -q get UAmask.main.ttl_ipv6 || echo 0)
    tmp="$RULE.$$"
    trap 'rm -f "$tmp"' EXIT HUP INT TERM
    render "$device" "$value" "$ipv6" >"$tmp"
    if [ ! -f "$STATE" ]; then
        umask 077
        { uci -q get 'firewall.@defaults[0].flow_offloading' || echo unset
          uci -q get 'firewall.@defaults[0].flow_offloading_hw' || echo unset
        } >"$STATE"
    fi
    # Flowtable/hardware paths can bypass postrouting on later packets.
    uci set 'firewall.@defaults[0].flow_offloading=0'
    uci set 'firewall.@defaults[0].flow_offloading_hw=0'
    uci commit firewall
    mv -f "$tmp" "$RULE"
    trap - EXIT HUP INT TERM
}

case "${1:-}" in
    render) [ "$#" = 4 ]; render "$2" "$3" "$4";;
    prepare) prepare;;
    restore) restore; rm -f "$RULE";;
    *) echo 'Usage: uamask-ttl {prepare|restore|render DEVICE TTL IPV6}' >&2; exit 2;;
esac
