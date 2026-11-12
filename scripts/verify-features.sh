#!/usr/bin/env bash
# Checks, against a real Docker engine, the features that were written
# where there was none: init.sh, the token volumes, logs, dev.Dockerfile,
# key attach, the ssh configuration, reset, rename, doctor.
#
#   scripts/verify-features.sh <bh-binary>
#
# It makes a throwaway environment of its own, in a configuration directory
# of its own (so it cannot see, let alone touch, your environments), and
# destroys it at the end. Takes a few minutes: it builds one small image.
#
# The boundary itself (LAN blocked, capabilities, namespaces) is the
# business of scripts/verify.sh; run that on the same environment with
#   BH_CONFIG_DIR=... scripts/verify.sh <bh-binary> verify-features
# by passing --keep here, which leaves the environment and prints where.
set -uo pipefail

BH=${1:?bh binary}
case "$BH" in /*) ;; *) BH=$PWD/$BH ;; esac
KEEP=${2:-}
NAME=verify-features
PORT=${BH_VERIFY_PORT:-23951}

WORK=$(mktemp -d)
export BH_CONFIG_DIR=$WORK/config
export BH_SSH_CONFIG=$WORK/ssh/config
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
check "…and it did run" bash -c "[ \"\$(docker exec bulkhead-$ID-dev wc -l < /home/developer/.bh-verify-init | tr -d ' ')\" = 2 ]"

echo "== token volumes =="
refuse "github volume is read-only inside" dev sh -c 'echo x > /etc/bulkhead/github/x'
refuse "tokens volume is read-only inside" dev sh -c 'echo x > /etc/bulkhead/tokens/x'
if "$BH" token 2>&1 | grep -q 'token set'; then
  check "token set (value on stdin)" bash -c "printf 'bh-verify-value' | \"$BH\" token set $NAME custom VERIFY_FEATURE_TOKEN"
  check "a new shell has the variable" bash -c "[ \"\$(docker exec bulkhead-$ID-dev zsh -c 'echo \$VERIFY_FEATURE_TOKEN')\" = bh-verify-value ]"
  check "the file belongs to the dev user, mode 600" bash -c "[ \"\$(docker exec bulkhead-$ID-dev stat -c '%u %a' /etc/bulkhead/tokens/env)\" = '1000 600' ]"
  check "token list names it" bash -c "\"$BH\" token list $NAME | grep -q VERIFY_FEATURE_TOKEN"
  check "token remove" "$BH" token remove "$NAME" VERIFY_FEATURE_TOKEN
  check "…and a new shell no longer has it" bash -c "[ -z \"\$(docker exec bulkhead-$ID-dev zsh -c 'echo \$VERIFY_FEATURE_TOKEN')\" ]"
else
  skip "bh token is not in this build"
fi
skip "bh github set needs a real token: check by hand"

echo "== logs =="
check "both containers, each line marked" bash -c "out=\$(\"$BH\" logs $NAME --no-follow --tail 30); grep -q '^holder | ' <<<\"\$out\" && grep -q '^dev    | ' <<<\"\$out\""
"$BH" logs "$NAME" --tail 1 >"$LOG" 2>&1 &
follower=$!
sleep 2
kill -INT "$follower" 2>/dev/null
if wait "$follower"; then ok "following ends cleanly on Ctrl-C"; else ng "following ended with status $? on Ctrl-C"; fi

echo "== status and doctor =="
check "status has a disk line" bash -c "\"$BH\" status $NAME | grep -q '^disk: '"
if "$BH" help | grep -q '^  doctor'; then
  check "doctor finds nothing wrong with it" "$BH" doctor "$NAME"
else
  skip "bh doctor is not in this build"
fi

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

echo "== reset =="
dev touch /home/developer/.bh-verify-home /home/developer/workspace/.bh-verify-workspace
fp_before=$(dev ssh-keygen -lf /etc/bulkhead/hostkeys/ssh_host_ed25519_key.pub | awk '{print $2}')
check "reset" "$BH" reset "$NAME" --yes
check "state is stopped or not created" bash -c "\"$BH\" status $NAME | grep -Eq '^state: *(stopped|not created)'"
check "the workspace volume is still there" bash -c "docker volume ls -q | grep -qx 'bulkhead-${ID}_workspace'"
refuse "the home volume is gone" bash -c "docker volume ls -q | grep -qx 'bulkhead-${ID}_home'"
check "up after reset" "$BH" up "$NAME"
refuse "home starts over" dev test -f /home/developer/.bh-verify-home
check "the workspace is kept" dev test -f /home/developer/workspace/.bh-verify-workspace
check "init.sh ran again in the fresh home" bash -c "[ \"\$(docker exec bulkhead-$ID-dev cat /home/developer/.bh-verify-init)\" = run ]"
fp_after=$(dev ssh-keygen -lf /etc/bulkhead/hostkeys/ssh_host_ed25519_key.pub | awk '{print $2}')
if [ -n "$fp_before" ] && [ "$fp_before" = "$fp_after" ]; then ok "ssh host key unchanged by reset"; else ng "ssh host key changed: $fp_before -> $fp_after"; fi

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
echo "By hand, at a terminal: bh (the full-screen interface), bh root-shell <env>,"
echo "bh open <env>, bh github set <env>, bh restart <env> --rebuild --pull --no-cache."
