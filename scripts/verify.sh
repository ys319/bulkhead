#!/usr/bin/env bash
# Smoke test of the boundary of a running environment: LAN blocked / WAN
# open / capability-less dev / ssh login / volumes that outlive the
# containers. It drives bh itself for the lifecycle parts.
#
# Ported from the TypeScript version's scripts/verify.sh; the checks are the
# same, only the way the containers are addressed changed (by name, not
# through compose).
#
#   scripts/verify.sh <bh-binary> <env>
#
# The environment must be up (`bh up <env>`). bh reads the configuration
# directory it would normally use (BH_CONFIG_DIR applies).
set -uo pipefail

BH=${1:?bh binary}
NAME=${2:?environment name}
status=$("$BH" status "$NAME") || exit 1
ID=$(awk "\$1 == \"id:\" {print \$2}" <<<"$status")
PORT=$(awk "\$1 == \"ssh:\" {print \$2}" <<<"$status" | sed "s/.*://")
KEY=$("$BH" path)/id_ed25519
if [ -z "$ID" ] || [ -z "$PORT" ] || [ ! -f "$KEY" ]; then
  echo "cannot read id / port / machine key of $NAME from bh" >&2
  exit 1
fi
HOLDER=bulkhead-$ID-holder
DEV=bulkhead-$ID-dev

# Git Bash (MSYS) rewrites arguments that look like absolute paths.
export MSYS_NO_PATHCONV=1

PASS=0
FAIL=0
SKIP=0
ok() { PASS=$((PASS + 1)); echo "  ok: $1"; }
ng() { FAIL=$((FAIL + 1)); echo "  NG: $1"; }
skip() { SKIP=$((SKIP + 1)); echo "  --: $1"; }
LOG=$(mktemp)
check() { # check <desc> <cmd...>: expect success
  if "${@:2}" >"$LOG" 2>&1; then ok "$1"; else ng "$1"; sed 's/^/      | /' "$LOG"; fi
}
dev() { docker exec "$DEV" "$@"; }
holder() { docker exec "$HOLDER" "$@"; }

# "The connection failed" does not prove "the packet was dropped" — an address
# with no listener fails either way. Blocking is asserted through the drop
# rules' packet counters instead.
holder_drop_count() { # packets counted by the DROP rule for <range>
  holder sh -c "iptables -w 2 -v -x -n -L OUTPUT" 2>/dev/null \
    | awk -v net="$1" '$3 == "DROP" && $NF == net {print $1; exit}'
}
assert_dropped() { # assert_dropped <probe-ip> <range>
  local before after
  before=$(holder_drop_count "$2")
  dev timeout 3 bash -c "echo > /dev/tcp/$1/80" >/dev/null 2>&1
  after=$(holder_drop_count "$2")
  if [ -z "$before" ] || [ -z "$after" ]; then
    ng "$1 dropped by $2 rule (could not read counters)"
  elif [ "$after" -gt "$before" ]; then
    ok "$1 swallowed by the $2 drop rule ($before -> $after)"
  else
    ng "$1 did NOT hit the $2 drop rule (counter stuck at $after)"
  fi
}

SIBLING=bh-verify-sibling-$ID
trap 'rm -f "$LOG"; docker rm -f "$SIBLING" >/dev/null 2>&1' EXIT

echo "== state =="
check "holder is running and healthy" bash -c "[ \"\$(docker inspect -f '{{.State.Health.Status}}' $HOLDER)\" = healthy ]"
check "dev is running" bash -c "[ \"\$(docker inspect -f '{{.State.Status}}' $DEV)\" = running ]"
check "dev joins the holder by container id" bash -c "[ \"\$(docker inspect -f '{{.HostConfig.NetworkMode}}' $DEV)\" = \"container:\$(docker inspect -f '{{.Id}}' $HOLDER)\" ]"

echo "== assertions =="
capeff=$(dev grep CapEff /proc/1/status | awk '{print $2}')
if [ "$capeff" = "0000000000000000" ]; then ok "dev CapEff is zero"; else ng "dev CapEff = $capeff"; fi

ns_holder=$(holder readlink /proc/1/ns/net)
ns_dev=$(dev readlink /proc/1/ns/net)
if [ -n "$ns_holder" ] && [ "$ns_holder" = "$ns_dev" ]; then
  ok "netns shared ($ns_holder)"
else
  ng "netns differ: holder=$ns_holder dev=$ns_dev"
fi

echo "-- blocked ranges (counter-based, from dev) --"
assert_dropped 10.255.255.1 10.0.0.0/8
assert_dropped 172.31.255.1 172.16.0.0/12
assert_dropped 192.168.255.1 192.168.0.0/16
assert_dropped 169.254.255.1 169.254.0.0/16

# The host sits at a private address, so it falls into the ranges above.
host_ip=$(dev getent ahosts host.docker.internal | awk '{print $1; exit}')
case "$host_ip" in
  10.*)      assert_dropped "$host_ip" 10.0.0.0/8 ;;
  172.1[6-9].*|172.2?.*|172.3[01].*) assert_dropped "$host_ip" 172.16.0.0/12 ;;
  192.168.*) assert_dropped "$host_ip" 192.168.0.0/16 ;;
  "")        skip "host.docker.internal does not resolve here — host reachability not checked" ;;
  *)         ng "host.docker.internal is $host_ip, outside every blocked range" ;;
esac

# A sibling on the same bridge needs a REAL listener: a failed connection to
# an empty address is not evidence of blocking.
sib_net=$(docker inspect "$HOLDER" --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}')
sib_img=$(docker inspect "$HOLDER" --format '{{.Config.Image}}')
docker rm -f "$SIBLING" >/dev/null 2>&1
if docker run -d --name "$SIBLING" --network "$sib_net" --entrypoint sh "$sib_img" \
     -c 'while :; do printf "SIBLING\n" | nc -l 8080; done' >/dev/null 2>&1 && sleep 2; then
  sib_ip=$(docker inspect "$SIBLING" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
  if dev timeout 4 bash -c "exec 3<>/dev/tcp/$sib_ip/8080" >/dev/null 2>&1; then
    ng "sibling $sib_ip:8080 unreachable from dev (REACHED — bridge is open)"
  else
    ok "sibling $sib_ip:8080 unreachable from dev"
  fi
else
  ng "sibling reachability (probe container would not start)"
fi
docker rm -f "$SIBLING" >/dev/null 2>&1

echo "-- allowed paths --"
check "WAN 1.1.1.1:443 reachable" dev timeout 5 bash -c 'echo > /dev/tcp/1.1.1.1/443'
check "DNS resolution works" dev getent hosts example.com
check "IPv6 disabled" dev bash -c '[ ! -s /proc/net/if_inet6 ]'

echo "-- invariants (threat model) --"
check "no docker socket in dev" dev bash -c '! test -e /var/run/docker.sock'
check "dev has no-new-privileges" dev bash -c 'grep -q "^NoNewPrivs:.1" /proc/1/status'
check "dev is not privileged" bash -c "[ \"\$(docker inspect -f '{{.HostConfig.Privileged}}' $DEV)\" = false ]"
check "published port lives on holder only" bash -c "[ -n \"\$(docker port $HOLDER 22 2>/dev/null)\" ] && [ -z \"\$(docker port $DEV 2>/dev/null)\" ]"
check "ssh is published on loopback only" bash -c "[ \"\$(docker port $HOLDER 22)\" = 127.0.0.1:$PORT ]"
check "holder keeps only NET_ADMIN" holder sh -c 'grep -q "^CapEff:.0000000000001000" /proc/1/status'
check "hostname is the environment name" bash -c "[ \"\$(docker exec $DEV hostname)\" = $NAME ]"

echo "-- ssh --"
# Through the published port, the way VS Code Remote SSH connects. Only
# possible where 127.0.0.1 is the Docker host (not from inside a container).
SSH="ssh -F /dev/null -p $PORT -i $KEY -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR developer@127.0.0.1"
if $SSH true >/dev/null 2>&1; then
  ok "ssh login via 127.0.0.1:$PORT"
  check "nix over ssh (sshd sets USER)" $SSH 'nix --version'
else
  skip "ssh via 127.0.0.1:$PORT not reachable from here (run on the Docker host to check the published port)"
fi
# Inside the namespace: proves sshd, the injected key and the login shell.
docker exec -i "$DEV" sh -c 'umask 077; cat > /tmp/bh-verify-key' < "$KEY"
ISSH="ssh -F /dev/null -p 22 -i /tmp/bh-verify-key -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR developer@127.0.0.1"
check "ssh login inside the namespace (sshd + injected key)" dev $ISSH true
check "glibc >= 2.28 (VS Code server requirement)" bash -c "docker exec $DEV $ISSH 'ldd --version' | head -1 | grep -Eq ' 2\.(2[89]|[3-9][0-9])'"
check "bash/tar/curl present (VS Code server requirement)" dev $ISSH 'command -v bash && command -v tar && command -v curl'
check "a key that was not injected is refused" bash -c "docker exec $DEV sh -c 'rm -f /tmp/bh-other /tmp/bh-other.pub; ssh-keygen -q -t ed25519 -N \"\" -f /tmp/bh-other' && ! docker exec $DEV ssh -F /dev/null -p 22 -i /tmp/bh-other -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR developer@127.0.0.1 true"
dev rm -f /tmp/bh-verify-key /tmp/bh-other /tmp/bh-other.pub >/dev/null 2>&1

echo "-- shell & toolchain --"
check "login shell is zsh" dev bash -c 'getent passwd developer | grep -q :/usr/bin/zsh$'
check "grml zshrc active in interactive zsh" dev zsh -i -c 'test -n "$GRML_OSTYPE"'
check "nix on PATH without USER set (plain exec path)" dev zsh -c 'nix --version'
check "nix flakes + nix-command enabled" dev zsh -c 'nix eval --expr 1+1'

echo "-- bh-guest --"
check "bh-guest installed (root-owned, 755)" dev zsh -c '[ "$(stat -c %U:%a /usr/local/bin/bh-guest)" = root:755 ] && bh-guest --version'
check "no GH_TOKEN without a token" dev zsh -c '[ -z "${GH_TOKEN:-}" ]'
check "/etc/gitconfig routes github.com to bh-guest" dev zsh -c '[ "$(git config --system --get credential.https://github.com.helper)" = "/usr/local/bin/bh-guest git-helper" ]'

echo "== down / up keeps the containers and the gate =="
id_holder=$(docker inspect -f '{{.Id}}' "$HOLDER")
id_dev=$(docker inspect -f '{{.Id}}' "$DEV")
check "bh down" "$BH" down "$NAME"
check "bh status says stopped" bash -c "\"$BH\" status $NAME | grep -q \"^state: *stopped\""
check "containers are kept, stopped" bash -c "[ \"\$(docker inspect -f '{{.State.Status}}' $HOLDER)\" = exited ] && [ \"\$(docker inspect -f '{{.State.Status}}' $DEV)\" = exited ]"
# The volumes bh mounts: the data, and the two the tokens are handed in by.
check "volumes stay referenced (not dangling)" bash -c "! docker volume ls -q --filter dangling=true | grep -Eq '^bulkhead-${ID}_(workspace|home|nix|hostkeys|github|tokens)$'"
check "bh up (same containers)" "$BH" up "$NAME"
check "same container ids after down/up" bash -c "[ \"\$(docker inspect -f '{{.Id}}' $HOLDER)\" = $id_holder ] && [ \"\$(docker inspect -f '{{.Id}}' $DEV)\" = $id_dev ]"
ns_holder=$(holder readlink /proc/1/ns/net)
ns_dev=$(dev readlink /proc/1/ns/net)
if [ -n "$ns_holder" ] && [ "$ns_holder" = "$ns_dev" ]; then ok "netns shared again after down/up"; else ng "netns differ after down/up: holder=$ns_holder dev=$ns_dev"; fi
check "WAN reachable again after down/up" dev timeout 5 bash -c 'echo > /dev/tcp/1.1.1.1/443'

echo "== a holder restarted on its own =="
# The ready marker must not outlive its run, and dev must not be left in the
# old namespace.
docker stop "$HOLDER" >/dev/null 2>&1
docker start "$HOLDER" >/dev/null 2>&1
if holder test -e /run/bulkhead/ready >/dev/null 2>&1; then
  ng "ready marker absent right after start (stale marker survived)"
else
  ok "ready marker absent right after start"
fi
check "bh status says degraded" bash -c "\"$BH\" status $NAME | grep -q \"^state: *degraded\""
check "bh up brings dev back behind the fresh holder" "$BH" up "$NAME"
check "bh status says running" bash -c "\"$BH\" status $NAME | grep -q \"^state: *running\""
check "holder re-asserted" holder /usr/local/bin/healthcheck.sh
ns_holder=$(holder readlink /proc/1/ns/net)
ns_dev=$(dev readlink /proc/1/ns/net)
if [ -n "$ns_holder" ] && [ "$ns_holder" = "$ns_dev" ]; then ok "netns shared after the holder restart"; else ng "netns differ after the holder restart: holder=$ns_holder dev=$ns_dev"; fi
check "WAN reachable after the holder restart" dev timeout 5 bash -c 'echo > /dev/tcp/1.1.1.1/443'

echo "== recreate persistence =="
dev touch /home/developer/workspace/.bh-verify-marker /nix/var/.bh-verify-marker
fp_before=$(dev ssh-keygen -lf /etc/bulkhead/hostkeys/ssh_host_ed25519_key.pub | awk '{print $2}')
check "bh restart recreates both" "$BH" restart "$NAME"
check "new container ids" bash -c "[ \"\$(docker inspect -f '{{.Id}}' $HOLDER)\" != $id_holder ] && [ \"\$(docker inspect -f '{{.Id}}' $DEV)\" != $id_dev ]"
check "workspace volume survives recreate" dev test -f /home/developer/workspace/.bh-verify-marker
check "nix volume survives recreate" dev test -f /nix/var/.bh-verify-marker
fp_after=$(dev ssh-keygen -lf /etc/bulkhead/hostkeys/ssh_host_ed25519_key.pub | awk '{print $2}')
if [ -n "$fp_before" ] && [ "$fp_before" = "$fp_after" ]; then ok "ssh host key stable across recreate"; else ng "ssh host key changed: $fp_before -> $fp_after"; fi
dev rm -f /home/developer/workspace/.bh-verify-marker /nix/var/.bh-verify-marker >/dev/null 2>&1

echo
echo "passed: $PASS, failed: $FAIL, skipped: $SKIP"
[ "$FAIL" -eq 0 ]
