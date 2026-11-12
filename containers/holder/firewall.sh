#!/bin/sh
# bulkhead netns holder entrypoint: fail-closed egress filter for the shared
# network namespace.
#
# DECIDED: what to block -> docs/decisions/0002-egress-scope.md
# DECIDED: why a separate holder container -> docs/decisions/0003-netns-holder.md
#
# Contract: the ready flag (/run/bulkhead/ready) MUST only appear after the
# self-assert has proven the filter bites. The compose healthcheck reads it and
# the dev container waits on service_healthy — that chain is the fail-safe gate.
set -eu

log() { echo "[holder] $*"; }

READY_DIR=/run/bulkhead

# A stale marker would make the healthcheck report healthy while this run's
# self-assert is still going — dev would then start past an unverified filter.
# /run is also a tmpfs in compose; this line keeps the contract if it is not.
rm -f "$READY_DIR/ready"

# Probe address / blocked range pairs for the self-assert: one probe per range.
# Since nothing gets a blanket accept any more, a probe that lands in this
# container's own bridge subnet is dropped like any other address in the range,
# so no collision handling is needed.
ASSERT_PAIRS="
10.255.255.1 10.0.0.0/8
172.31.255.1 172.16.0.0/12
192.168.255.1 192.168.0.0/16
169.254.255.1 169.254.0.0/16
"

ASSERT_WAN_HOST="${ASSERT_WAN_HOST:-1.1.1.1}"
ASSERT_WAN_PORT="${ASSERT_WAN_PORT:-443}"
ASSERT_DNS_NAME="${ASSERT_DNS_NAME:-example.com}"

install_rules() { # install_rules <iptables-cmd>
  ipt="$1"
  # MUST set DROP policy before any ACCEPT rule: if installation dies partway,
  # the namespace stays closed instead of wide open.
  "$ipt" -w -P OUTPUT DROP || return 1
  "$ipt" -w -P FORWARD DROP || return 1
  "$ipt" -w -A OUTPUT -o lo -j ACCEPT || return 1
  # Conntrack MUST precede the private-range drops: replies to published ports
  # leave through the bridge subnet, which the drops would otherwise eat.
  # This alone covers them — a blanket accept for the own subnet would also
  # open every sibling container on the bridge (measured 2026-07-27).
  "$ipt" -w -A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT || return 1
  for ns in $RESOLVERS; do
    "$ipt" -w -A OUTPUT -d "$ns" -p udp --dport 53 -j ACCEPT || return 1
    "$ipt" -w -A OUTPUT -d "$ns" -p tcp --dport 53 -j ACCEPT || return 1
  done
  for net in $BLOCKED_NETS; do
    "$ipt" -w -A OUTPUT -d "$net" -j DROP || return 1
  done
  "$ipt" -w -A OUTPUT -j ACCEPT || return 1

  # Defense in depth. IPv6 is off (sysctl) and assert_all proves it, but closing
  # the v6 policy too means a later activation fails shut instead of bypassing
  # the whole v4 ruleset — the one full-bypass route ADR 0002 names.
  ipt6=$(printf '%s' "$ipt" | sed 's/^iptables/ip6tables/')
  if command -v "$ipt6" >/dev/null 2>&1 && "$ipt6" -w -P OUTPUT DROP 2>/dev/null; then
    "$ipt6" -w -P FORWARD DROP 2>/dev/null || true
    "$ipt6" -w -P INPUT DROP 2>/dev/null || true
    log "IPv6 policy closed via $ipt6"
  else
    # Not fatal: the hard guarantee is the assert below, not this policy.
    log "WARNING: $ipt6 unusable — IPv6 rests on the sysctl and the assert alone"
  fi
}

assert_ranges() { # probe each blocked range and require its DROP rule to count
  ipt="$1"
  rc=0
  "$ipt" -w -Z OUTPUT
  while read -r probe net; do
    [ -n "$probe" ] || continue
    if nc -z -w 1 "$probe" 80 2>/dev/null; then
      log "ASSERT FAIL: private address $probe:80 is reachable"
      rc=1
      continue
    fi
    # The counter check works without any real LAN host: a DROP rule eats the
    # SYN whether or not something answers at the address.
    pkts=$("$ipt" -w -v -x -n -L OUTPUT 2>/dev/null \
      | awk -v net="$net" '$3 == "DROP" && $9 == net {print $1; exit}')
    if [ "${pkts:-0}" -ge 1 ] 2>/dev/null; then
      log "assert ok: probe $probe swallowed by the $net drop rule"
    else
      log "ASSERT FAIL: drop rule for $net counted no packets"
      rc=1
    fi
  done <<EOF
$ASSERT_PAIRS
EOF
  return $rc
}

assert_all() { # assert_all <iptables-cmd>
  ipt="$1"
  rc=0
  assert_ranges "$ipt" || rc=1
  # IPv6 up means every rule above is bypassable, so this is a gate condition,
  # not a warning (ADR 0002).
  if [ -s /proc/net/if_inet6 ]; then
    log "ASSERT FAIL: IPv6 is up — the IPv4 ruleset can be bypassed"
    rc=1
  else
    log "assert ok: IPv6 is down"
  fi
  if nc -z -w 5 "$ASSERT_WAN_HOST" "$ASSERT_WAN_PORT" 2>/dev/null; then
    log "assert ok: WAN $ASSERT_WAN_HOST:$ASSERT_WAN_PORT reachable"
  else
    log "ASSERT FAIL: WAN $ASSERT_WAN_HOST:$ASSERT_WAN_PORT unreachable"
    rc=1
  fi
  if nslookup "$ASSERT_DNS_NAME" >/dev/null 2>&1; then
    log "assert ok: DNS resolves $ASSERT_DNS_NAME"
  else
    log "ASSERT FAIL: DNS cannot resolve $ASSERT_DNS_NAME"
    rc=1
  fi
  return $rc
}

# Diagnostic only — no rule keys off the bridge subnet any more. Worth logging
# because "which addresses are my siblings" is the first question when egress
# behaviour surprises someone.
IFACE=$(ip -o -4 route show default | awk '{print $5; exit}')
log "bridge subnet: $(ip -o -4 addr show dev "$IFACE" 2>/dev/null | awk '{print $4; exit}') (dev $IFACE)"

# Resolver address differs per platform (127.0.0.11 on user-defined networks,
# 192.168.65.x on the Docker Desktop default bridge) — read it, never hardcode.
RESOLVERS=$(awk '/^nameserver/ {print $2}' /etc/resolv.conf | grep -v ':' || true)
[ -n "$RESOLVERS" ] || { log "FATAL: no IPv4 nameserver in /etc/resolv.conf"; exit 1; }

BLOCKED_NETS=$(printf '%s\n' "$ASSERT_PAIRS" | awk 'NF {print $2}')

# One backend: the image ships only the nft-based iptables, and nft is verified
# working on both target platforms (macOS 2026-07-27, Windows/WSL2 2026-07-28).
#
# If some future platform needs the legacy backend, reintroducing a walk over
# candidates takes more than adding the package: a failed attempt leaves its
# rules in place, and the next candidate's counter check then reads zero because
# the earlier backend already swallowed the probe. Flush between attempts —
# with the policy left at DROP so the flush itself cannot fail open.
IPT=iptables
install_rules "$IPT" || { log "FATAL: could not install the filter — gate stays closed"; exit 1; }
assert_all "$IPT" || { log "FATAL: self-assert failed — gate stays closed"; exit 1; }

mkdir -p "$READY_DIR"
: > "$READY_DIR/ready"
log "ready: filter verified via $IPT — gate open"
exec tail -f /dev/null
