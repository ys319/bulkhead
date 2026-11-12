#!/bin/sh
# Healthy = this run's firewall.sh wrote the ready flag (which only happens
# after a passed self-assert) AND the filter is still intact. The flag lives
# on a tmpfs and is removed at entrypoint start, so it cannot outlive its run.
# It is content-free on purpose: the backend is fixed to the nft-based
# iptables (see firewall.sh), so nothing may execute what a file contains.
set -eu

[ -e /run/bulkhead/ready ]
IPT=iptables

# Policy first: it is the fail-closed floor. Rules above it are worthless if the
# default went back to ACCEPT.
"$IPT" -w 2 -S OUTPUT | head -1 | grep -qx -- '-P OUTPUT DROP'

# Every range firewall.sh installs — checking a subset would let the rest be
# removed silently.
for net in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16; do
  "$IPT" -w 2 -C OUTPUT -d "$net" -j DROP
done

# IPv6 coming up would bypass all of the above (ADR 0002).
[ ! -s /proc/net/if_inet6 ]
