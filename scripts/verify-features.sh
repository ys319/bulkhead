#!/usr/bin/env bash
# Checks, against a real Docker engine, the features that were written
# where there was none: init.sh, variables and secrets, logs, dev.Dockerfile,
# key attach, the ssh configuration, reset, rename, doctor.
#
#   scripts/verify-features.sh <bh-binary>
#
# It makes a throwaway environment of its own, in a configuration directory
# of its own (so it cannot see, let alone touch, your environments), and
# destroys it at the end. Takes a few minutes: it builds one small image.
#
# The boundary itself (LAN blocked, capabilities, namespaces) is the
# business of scripts/verify.sh. When that script sits next to this one it
# is run at the end, on the same environment: the features above changed
# what dev mounts, and the boundary has to hold with them.
#
# --keep leaves the environment and prints where.
set -uo pipefail

BH=${1:?bh binary}
case "$BH" in /*) ;; *) BH=$PWD/$BH ;; esac
BOUNDARY=$(cd "$(dirname "$0")" && pwd)/verify.sh
KEEP=${2:-}
NAME=verify-features
PORT=${BH_VERIFY_PORT:-23951}

# Without a directory of its own nothing below may run: the configuration
# would be /config, a place this script did not make and does not own.
WORK=$(mktemp -d) && [ -d "$WORK" ] || { echo "cannot make a work directory" >&2; exit 1; }
export BH_CONFIG_DIR=$WORK/config
export BH_SSH_CONFIG=$WORK/ssh/config
# Values of secrets go to a file in that directory, not to your keychain
# (one section asks for the keychain itself: BH_VERIFY_KEYCHAIN=1).
export BH_SECRET_STORE=file
# Git Bash (MSYS) rewrites arguments that look like absolute paths.
export MSYS_NO_PATHCONV=1

PASS=0
FAIL=0
SKIP=0
ok() { PASS=$((PASS + 1)); echo "  ok: $1"; }
ng() { FAIL=$((FAIL + 1)); echo "  NG: $1"; }
skip() { SKIP=$((SKIP + 1)); echo "  --: $1"; }
LOG=$WORK/last.log
check() { # check <desc> <cmd...>: expect success
  if "${@:2}" >"$LOG" 2>&1; then ok "$1"; else ng "$1"; sed 's/^/      | /' "$LOG"; fi
}
refuse() { # refuse <desc> <cmd...>: expect failure
  if "${@:2}" >"$LOG" 2>&1; then ng "$1"; sed 's/^/      | /' "$LOG"; else ok "$1"; fi
}

CURRENT=$NAME # the environment's name now (it is renamed along the way)
ID=
finish() {
  echo
  if [ "$KEEP" = "--keep" ]; then
    echo "kept: BH_CONFIG_DIR=$BH_CONFIG_DIR ($CURRENT)"
    echo "remove it with: BH_CONFIG_DIR=$BH_CONFIG_DIR BH_SSH_CONFIG=$BH_SSH_CONFIG $BH destroy $CURRENT --yes"
  else
    "$BH" destroy "$CURRENT" --yes >/dev/null 2>&1
    if [ -n "$ID" ]; then
      left=$(docker ps -aq --filter "name=bulkhead-$ID-" | wc -l | tr -d ' ')
      left_volumes=$(docker volume ls -q --filter "name=bulkhead-${ID}_" | wc -l | tr -d ' ')
      if [ "$left" = 0 ] && [ "$left_volumes" = 0 ]; then ok "destroy left no container and no volume"; else ng "destroy left $left container(s) and $left_volumes volume(s) of bulkhead-$ID"; fi
      images=$(docker image ls -q "bulkhead-dev-$ID" | sort -u)
      [ -n "$images" ] && docker rmi $images >/dev/null 2>&1
    fi
    rm -rf "$WORK"
  fi
  echo "passed: $PASS, failed: $FAIL, skipped: $SKIP"
  [ "$FAIL" -eq 0 ]
}
trap 'finish; exit $?' EXIT

dev() { docker exec "bulkhead-$ID-dev" "$@"; }
# A login the way a user's ssh client does it, without touching known_hosts.
login() { # login <private key> <command...>
  ssh -i "$1" -p "$PORT" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR developer@127.0.0.1 "${@:2}"
}

echo "== start =="
check "engine reachable" "$BH" version
check "create" "$BH" create "$NAME" --port "$PORT"
ID=$("$BH" status "$NAME" 2>/dev/null | awk '$1 == "id:" {print $2}')
if [ -z "$ID" ]; then
  echo "cannot read the id of $NAME from bh status" >&2
  exit 1
fi
MACHINE_KEY=$("$BH" path)/id_ed25519

# init.sh is shared by all environments of a configuration directory; this
# directory is ours alone. The file must exist before the first up.
"$BH" key show >/dev/null 2>&1
cat >"$BH_CONFIG_DIR/init.sh" <<'EOF'
#!/usr/bin/env zsh
echo run >> ~/.bh-verify-init
EOF
chmod 755 "$BH_CONFIG_DIR/init.sh"
check "up (builds the images the first time)" "$BH" up "$NAME"
check "state is running" bash -c "\"$BH\" status $NAME | grep -q '^state: *running'"

echo "== init.sh =="
check "ran once at the first boot" bash -c "[ \"\$(docker exec bulkhead-$ID-dev cat /home/developer/.bh-verify-init)\" = run ]"
refuse "cannot be changed from inside" dev sh -c 'echo x >> /etc/bulkhead/init.sh'
check "bh init runs it again" "$BH" init "$NAME"
check "…and it did run" bash -c "[ \"\$(docker exec bulkhead-$ID-dev sh -c 'wc -l < /home/developer/.bh-verify-init' | tr -d ' ')\" = 2 ]"

echo "== variables and secrets =="
refuse "github volume is read-only inside" dev sh -c 'echo x > /etc/bulkhead/github/x'
refuse "tokens volume is read-only inside" dev sh -c 'echo x > /etc/bulkhead/tokens/x'
# In a new shell inside, the variable as it is: printed without a line break.
inside() { docker exec "bulkhead-$ID-dev" zsh -c "printf %s \"\$$1\""; }
same() { # same <desc> <got> <want>
  if [ "$2" = "$3" ]; then ok "$1"; else ng "$1"; echo "      | got:  $2"; echo "      | want: $3"; fi
}
VALUE="it's a \"value\" with  spaces & \$HOME \$(echo no)"
check "var set" "$BH" var set "$NAME" BH_VERIFY_VAR "$VALUE"
same "a new shell has the variable, character for character" "$(inside BH_VERIFY_VAR)" "$VALUE"
check "var list shows it" bash -c "\"$BH\" var list $NAME | grep -q '^BH_VERIFY_VAR='"
check "a lower-case name works too" "$BH" var set "$NAME" bh_verify_lower plain
same "…and reaches a new shell" "$(inside bh_verify_lower)" plain
check "secret set (value on stdin)" bash -c "printf 'bh-verify-secret' | \"$BH\" secret set $NAME BH_VERIFY_SECRET"
same "a new shell has the secret" "$(inside BH_VERIFY_SECRET)" bh-verify-secret
check "the file inside belongs to the dev user, mode 600" bash -c "[ \"\$(docker exec bulkhead-$ID-dev stat -c '%u %a' /etc/bulkhead/tokens/env)\" = '1000 600' ]"
check "secret list names it and does not show it" bash -c "out=\$(\"$BH\" secret list $NAME); grep -q BH_VERIFY_SECRET <<<\"\$out\" && ! grep -q bh-verify-secret <<<\"\$out\""
check "the records do not hold the value" bash -c "! grep -q bh-verify-secret \"$BH_CONFIG_DIR/environments/$ID/secrets.json\" \"$BH_CONFIG_DIR/environments/$ID/env.json\""
check "the file that does is private (600)" bash -c "[ \"\$(ls -l \"$BH_CONFIG_DIR/environments/$ID/values.json\" | cut -c1-10)\" = '-rw-------' ]"
check "var unset" "$BH" var unset "$NAME" BH_VERIFY_VAR
check "var unset (the other)" "$BH" var unset "$NAME" bh_verify_lower
check "secret remove" "$BH" secret remove "$NAME" BH_VERIFY_SECRET
same "…and a new shell has none of them" "$(inside BH_VERIFY_VAR)$(inside bh_verify_lower)$(inside BH_VERIFY_SECRET)" ""
refuse "the last secret took the values file with it" test -e "$BH_CONFIG_DIR/environments/$ID/values.json"
skip "bh token set needs a real token: check by hand (bh token set <env>)"

# The credential store of this machine — your own keychain. Off unless
# asked for: BH_VERIFY_KEYCHAIN=1. It puts one value in and takes it out.
if [ "${BH_VERIFY_KEYCHAIN:-}" = 1 ]; then
  echo "== the credential store (BH_VERIFY_KEYCHAIN=1) =="
  real() { env -u BH_SECRET_STORE "$@"; }
  check "secret set, with the store left to bh" bash -c "printf 'bh-verify-keychain' | env -u BH_SECRET_STORE \"$BH\" secret set $NAME BH_VERIFY_KEYCHAIN_SECRET"
  check "bh chose the credential store" bash -c "env -u BH_SECRET_STORE \"$BH\" secret store | grep -q '(keychain)'"
  check "no file under the configuration directory holds the value" bash -c "! grep -rq bh-verify-keychain \"$BH_CONFIG_DIR\""
  same "a new shell has it" "$(inside BH_VERIFY_KEYCHAIN_SECRET)" bh-verify-keychain
  check "restart reads it back from the store" real "$BH" restart "$NAME"
  same "…and hands it in again" "$(inside BH_VERIFY_KEYCHAIN_SECRET)" bh-verify-keychain
  OWNER=$(sed -n 's/.*"id": *"\([^"]*\)".*/\1/p' "$BH_CONFIG_DIR/bulkhead.json")
  ACCOUNT="$OWNER/$ID/secret/BH_VERIFY_KEYCHAIN_SECRET"
  if command -v security >/dev/null 2>&1; then
    check "the item is in the keychain, under bulkhead" security find-generic-password -s bulkhead -a "$ACCOUNT"
  fi
  check "secret remove" real "$BH" secret remove "$NAME" BH_VERIFY_KEYCHAIN_SECRET
  if command -v security >/dev/null 2>&1; then
    refuse "…and the item is gone from the keychain" security find-generic-password -s bulkhead -a "$ACCOUNT"
  fi
else
  skip "the credential store: BH_VERIFY_KEYCHAIN=1 checks it with your own keychain (one value in, and out again)"
fi

echo "== logs =="
check "both containers, each line marked" bash -c "out=\$(\"$BH\" logs $NAME --no-follow --tail 30); grep -q '^holder | ' <<<\"\$out\" && grep -q '^dev    | ' <<<\"\$out\""
"$BH" logs "$NAME" --tail 1 >"$LOG" 2>&1 &
follower=$!
sleep 2
kill -INT "$follower" 2>/dev/null
if wait "$follower"; then ok "following ends cleanly on Ctrl-C"; else ng "following ended with status $? on Ctrl-C"; fi

echo "== status and doctor =="
check "status has a disk line" bash -c "\"$BH\" status $NAME | grep -q '^disk: '"
check "doctor finds nothing wrong with it" "$BH" doctor "$NAME"

echo "== ssh =="
check "login with the machine key" login "$MACHINE_KEY" true
check "the Host entry bh wrote works" ssh -F "$WORK/ssh/bh/config" -o BatchMode=yes -o StrictHostKeyChecking=no \
  -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "bulkhead-$NAME" true
ssh-keygen -q -t ed25519 -N '' -C verify-extra -f "$WORK/extra" >/dev/null
refuse "a key that is not attached is turned away" login "$WORK/extra" true
check "key add" bash -c "\"$BH\" key add --label verify-extra < \"$WORK/extra.pub\""
check "key attach" "$BH" key attach "$NAME" verify-extra
check "up applies it" "$BH" up "$NAME"
check "the attached key gets in" login "$WORK/extra" true
check "key detach" "$BH" key detach "$NAME" verify-extra
check "up applies that too" "$BH" up "$NAME"
refuse "the detached key is turned away again" login "$WORK/extra" true

echo "== dev.Dockerfile =="
ENV_DIR=$("$BH" path "$NAME")
cat >"$ENV_DIR/dev.Dockerfile" <<'EOF'
FROM bulkhead-dev-base
USER root
RUN touch /etc/bh-verify-custom
USER developer
EOF
check "up builds the environment's own image" "$BH" up "$NAME"
check "dev runs from it" bash -c "docker inspect -f '{{.Config.Image}}' bulkhead-$ID-dev | grep -q '^bulkhead-dev-$ID:'"
check "its build step is in the container" dev test -f /etc/bh-verify-custom
check "still no capabilities" bash -c "[ \"\$(docker exec bulkhead-$ID-dev grep CapEff /proc/1/status | awk '{print \$2}')\" = 0000000000000000 ]"
rm -f "$ENV_DIR/dev.Dockerfile"
check "without the file, up goes back to the base image" "$BH" up "$NAME"
check "dev runs from the base" bash -c "docker inspect -f '{{.Config.Image}}' bulkhead-$ID-dev | grep -q '^bulkhead-dev-base:'"

echo "== reset: the tools =="
dev sh -c 'touch /home/developer/.bh-verify-home /home/developer/workspace/.bh-verify-workspace /nix/var/.bh-verify-nix && mkdir -p /home/developer/.cache && echo x > /home/developer/.cache/bh-verify'
fp_before=$(dev ssh-keygen -lf /etc/bulkhead/hostkeys/ssh_host_ed25519_key.pub | awk '{print $2}')
inits_before=$(dev sh -c 'wc -l < /home/developer/.bh-verify-init' | tr -d ' ')
check "reset" "$BH" reset "$NAME" --yes
check "state is stopped or not created" bash -c "\"$BH\" status $NAME | grep -Eq '^state: *(stopped|not created)'"
check "the home volume is still there" bash -c "docker volume ls -q | grep -qx 'bulkhead-${ID}_home'"
check "the workspace volume is still there" bash -c "docker volume ls -q | grep -qx 'bulkhead-${ID}_workspace'"
refuse "the nix volume is gone" bash -c "docker volume ls -q | grep -qx 'bulkhead-${ID}_nix'"
check "up after reset" "$BH" up "$NAME"
check "what was in the home directory is kept" dev test -f /home/developer/.bh-verify-home
check "the workspace is kept" dev test -f /home/developer/workspace/.bh-verify-workspace
refuse "the cache is gone" dev test -e /home/developer/.cache/bh-verify
refuse "the nix store started over" dev test -e /nix/var/.bh-verify-nix
check "nix works in the fresh store" dev zsh -c 'nix --version && nix eval --expr 1+1'
check "the nix profile link is the developer's" bash -c "[ \"\$(docker exec bulkhead-$ID-dev stat -c '%u:%g' /home/developer/.nix-profile)\" = '1000:1000' ]"
check "init.sh ran again" bash -c "[ \"\$(docker exec bulkhead-$ID-dev sh -c 'wc -l < /home/developer/.bh-verify-init' | tr -d ' ')\" = $((inits_before + 1)) ]"
check "doctor finds nothing wrong after it" "$BH" doctor "$NAME"

echo "== reset: the home directory too =="
check "reset --home" "$BH" reset "$NAME" --home --yes
check "the home directory went into a snapshot first" bash -c "ls \"$BH_CONFIG_DIR/snapshots/$ID\" | grep -q 'tar.gz$'"
check "the workspace volume is still there" bash -c "docker volume ls -q | grep -qx 'bulkhead-${ID}_workspace'"
refuse "the home volume is gone" bash -c "docker volume ls -q | grep -qx 'bulkhead-${ID}_home'"
check "up after reset" "$BH" up "$NAME"
refuse "home starts over" dev test -f /home/developer/.bh-verify-home
check "the workspace is kept" dev test -f /home/developer/workspace/.bh-verify-workspace
check "init.sh ran in the fresh home" bash -c "[ \"\$(docker exec bulkhead-$ID-dev cat /home/developer/.bh-verify-init)\" = run ]"
check "nix works" dev zsh -c 'nix --version'
fp_after=$(dev ssh-keygen -lf /etc/bulkhead/hostkeys/ssh_host_ed25519_key.pub | awk '{print $2}')
if [ -n "$fp_before" ] && [ "$fp_before" = "$fp_after" ]; then ok "ssh host key unchanged by the resets"; else ng "ssh host key changed: $fp_before -> $fp_after"; fi

echo "== rename =="
id_dev=$(docker inspect -f '{{.Id}}' "bulkhead-$ID-dev")
check "rename" "$BH" rename "$NAME" verify-renamed
CURRENT=verify-renamed
check "the same container, under the new name" bash -c "[ \"\$(docker inspect -f '{{.Id}}' bulkhead-$ID-dev)\" = $id_dev ] && \"$BH\" status verify-renamed | grep -q '^state: *running'"
check "the Host entry follows" grep -q '^Host bulkhead-verify-renamed$' "$WORK/ssh/bh/config"
check "restart recreates with the new hostname" "$BH" restart verify-renamed
check "hostname inside" bash -c "[ \"\$(docker exec bulkhead-$ID-dev hostname)\" = verify-renamed ]"

echo "== down =="
check "down --all stops it" "$BH" down --all
check "state is stopped" bash -c "\"$BH\" status verify-renamed | grep -q '^state: *stopped'"

echo
if [ -f "$BOUNDARY" ]; then
  echo "== the boundary (verify.sh, its own report follows) =="
  check "up again" "$BH" up verify-renamed
  if bash "$BOUNDARY" "$BH" verify-renamed; then ok "verify.sh passed"; else ng "verify.sh reported failures (above)"; fi
else
  skip "verify.sh is not next to this script: the boundary was not checked"
fi

echo
echo "By hand, at a terminal: bh (the full-screen interface), bh root-shell <env>,"
echo "bh open <env>, bh token set <env>, bh restart <env> --rebuild --pull --no-cache."
