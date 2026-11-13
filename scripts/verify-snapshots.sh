#!/usr/bin/env bash
# Checks snapshots against a real Docker engine: snapshot, rollback,
# export / import, archive / unarchive.
#
#   scripts/verify-snapshots.sh <bh-binary> [--keep]
#
# Like verify-features.sh it works in a configuration directory of its own
# and on throwaway environments of its own (verify-snap and the one it
# imports), and destroys them at the end. It cannot see your environments.
# Takes a few minutes: nix starts from the image twice.
set -uo pipefail

BH=${1:?bh binary}
case "$BH" in /*) ;; *) BH=$PWD/$BH ;; esac
KEEP=${2:-}
NAME=verify-snap
COPY=verify-snap-copy
PORT=${BH_VERIFY_PORT:-23961}
COPY_PORT=$((PORT + 1))

# Without a directory of its own nothing below may run.
WORK=$(mktemp -d) && [ -d "$WORK" ] || { echo "cannot make a work directory" >&2; exit 1; }
export BH_CONFIG_DIR=$WORK/config
export BH_SSH_CONFIG=$WORK/ssh/config
export MSYS_NO_PATHCONV=1

PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); echo "  ok: $1"; }
ng() { FAIL=$((FAIL + 1)); echo "  NG: $1"; }
LOG=$WORK/last.log
check() { # check <desc> <cmd...>: expect success
  if "${@:2}" >"$LOG" 2>&1; then ok "$1"; else ng "$1"; sed 's/^/      | /' "$LOG"; fi
}
refuse() { # refuse <desc> <cmd...>: expect failure
  if "${@:2}" >"$LOG" 2>&1; then ng "$1"; sed 's/^/      | /' "$LOG"; else ok "$1"; fi
}
is() { # is <desc> <got> <want>
  if [ "$2" = "$3" ]; then ok "$1"; else ng "$1 — got \"$2\", want \"$3\""; fi
}

ID=
COPY_ID=
finish() {
  echo
  if [ "$KEEP" = "--keep" ]; then
    echo "kept: BH_CONFIG_DIR=$BH_CONFIG_DIR BH_SSH_CONFIG=$BH_SSH_CONFIG"
  else
    "$BH" destroy "$NAME" --yes >/dev/null 2>&1
    "$BH" destroy "$COPY" --yes >/dev/null 2>&1
    for id in $ID $COPY_ID; do
      left=$(docker ps -aq --filter "name=bulkhead-$id-" | wc -l | tr -d ' ')
      left_volumes=$(docker volume ls -q --filter "name=bulkhead-${id}_" | wc -l | tr -d ' ')
      if [ "$left" = 0 ] && [ "$left_volumes" = 0 ]; then ok "destroy left nothing of $id"; else ng "destroy left $left container(s) and $left_volumes volume(s) of bulkhead-$id"; fi
    done
    rm -rf "$WORK"
  fi
  echo "passed: $PASS, failed: $FAIL"
  [ "$FAIL" -eq 0 ]
}
trap 'finish; exit $?' EXIT

# A login the way a user's ssh client does it, without touching known_hosts
# (bh ssh would write to the real one).
login() { # login <port>
  ssh -i "$("$BH" path)/id_ed25519" -p "$1" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR developer@127.0.0.1 true
}
# In the dev container of an environment, as the developer.
dev() { docker exec "bulkhead-$1-dev" zsh -c "$2"; }
id_of() { "$BH" status "$1" 2>/dev/null | awk '$1 == "id:" {print $2}'; }
volumes_of() { docker volume ls -q --filter "name=bulkhead-${1}_" | sed "s/^bulkhead-${1}_//" | sort | tr '\n' ' ' | sed 's/ $//'; }
snapshots_in() { ls "$BH_CONFIG_DIR/snapshots/$1" 2>/dev/null | wc -l | tr -d ' '; }

echo "== an environment with data =="
check "create" "$BH" create "$NAME" --port "$PORT"
ID=$(id_of "$NAME")
[ -n "$ID" ] || { echo "cannot read the id of $NAME" >&2; exit 1; }
"$BH" key show >/dev/null 2>&1
cat >"$BH_CONFIG_DIR/init.sh" <<'EOF'
#!/usr/bin/env zsh
echo run >> ~/.bh-verify-init
EOF
chmod 755 "$BH_CONFIG_DIR/init.sh"
check "up" "$BH" up "$NAME"
check "data in workspace and home" dev "$ID" '
  set -e
  cd ~/workspace
  echo v1 > notes.txt
  echo gone-later > removed-later.txt
  mkdir -p "dir with space/private" && chmod 700 "dir with space/private"
  echo secret > "dir with space/private/token" && chmod 600 "dir with space/private/token"
  echo one > one && ln one two
  ln -s /nix/store/does-not-exist dangling
  echo v1 > ~/.bh-verify-data
  mkdir -p ~/.cache && echo old-cache > ~/.cache/bh-verify
'
NIX_BEFORE=$(dev "$ID" 'nix --version')
PROFILE_BEFORE=$(dev "$ID" 'readlink -f ~/.nix-profile')
[ -n "$NIX_BEFORE" ] && ok "nix works ($NIX_BEFORE)" || ng "nix does not work before anything was done"

echo "== snapshot =="
refuse "a running environment is refused" "$BH" snapshot create "$NAME"
check "down" "$BH" down "$NAME"
check "snapshot create" "$BH" snapshot create "$NAME" --note first
FIRST=$(ls "$BH_CONFIG_DIR/snapshots/$ID" | head -1)
is "one snapshot, no partial file" "$(ls "$BH_CONFIG_DIR/snapshots/$ID" | tr '\n' ' ')" "$FIRST "
check "the file is private (600)" bash -c "[ \"\$(ls -l \"$BH_CONFIG_DIR/snapshots/$ID/$FIRST\" | cut -c1-10)\" = '-rw-------' ]"
check "snapshot list shows it" bash -c "\"$BH\" snapshot list $NAME | grep -q 'first'"
check "the caches are not in it" bash -c "! gzip -dc \"$BH_CONFIG_DIR/snapshots/$ID/$FIRST\" | tar -tf - | grep -q 'volumes/home/.cache/'"
check "the nix profile is not in it" bash -c "! gzip -dc \"$BH_CONFIG_DIR/snapshots/$ID/$FIRST\" | tar -tf - | grep -q 'volumes/home/.local/state/nix'"
check "the workspace is in it" bash -c "gzip -dc \"$BH_CONFIG_DIR/snapshots/$ID/$FIRST\" | tar -tf - | grep -q 'volumes/workspace/notes.txt'"

echo "== rollback =="
check "up" "$BH" up "$NAME"
check "work goes on" dev "$ID" '
  set -e
  cd ~/workspace
  echo v2 > notes.txt
  rm removed-later.txt
  echo new > added-later.txt
  echo v2 > ~/.bh-verify-data
  echo new-cache > ~/.cache/bh-verify
'
INIT_BEFORE=$(dev "$ID" 'wc -l < ~/.bh-verify-init | tr -d " "')
check "down" "$BH" down "$NAME"
sleep 1 # the next snapshot gets a later name
check "rollback latest" "$BH" rollback "$NAME" latest --yes
is "what was there is kept in a second snapshot" "$(snapshots_in "$ID")" 2
check "up" "$BH" up "$NAME"
is "workspace: the old contents" "$(dev "$ID" 'cat ~/workspace/notes.txt')" v1
is "workspace: a removed file is back" "$(dev "$ID" 'cat ~/workspace/removed-later.txt')" gone-later
refuse "workspace: a later file is gone" dev "$ID" 'test -e ~/workspace/added-later.txt'
is "home: the old contents" "$(dev "$ID" 'cat ~/.bh-verify-data')" v1
is "home: the cache was left alone" "$(dev "$ID" 'cat ~/.cache/bh-verify')" new-cache
is "owner and mode of a private file" "$(dev "$ID" 'stat -c "%u:%g %a" "$HOME/workspace/dir with space/private/token"')" "1000:1000 600"
is "owner and mode of a private directory" "$(dev "$ID" 'stat -c "%u:%g %a" "$HOME/workspace/dir with space/private"')" "1000:1000 700"
is "two names of one file are one file" "$(dev "$ID" '[ "$(stat -c %i ~/workspace/one)" = "$(stat -c %i ~/workspace/two)" ] && echo same')" same
is "a link that leads nowhere" "$(dev "$ID" 'readlink ~/workspace/dangling')" /nix/store/does-not-exist
is "the home directory is the developer's" "$(dev "$ID" 'stat -c "%u:%g" ~')" "1000:1000"
is "nix still works" "$(dev "$ID" 'nix --version')" "$NIX_BEFORE"
is "the nix profile is the one that was there" "$(dev "$ID" 'readlink -f ~/.nix-profile')" "$PROFILE_BEFORE"
is "init.sh did not run again" "$(dev "$ID" 'wc -l < ~/.bh-verify-init | tr -d " "')" "$INIT_BEFORE"
check "ssh login still works" login "$PORT"

echo "== rollback of one volume =="
SECOND=$(ls "$BH_CONFIG_DIR/snapshots/$ID" | tail -1)
check "down" "$BH" down "$NAME"
check "rollback --only workspace, to the state before the first rollback" "$BH" rollback "$NAME" "$SECOND" --only workspace --no-snapshot --yes
is "--no-snapshot took none" "$(snapshots_in "$ID")" 2
check "up" "$BH" up "$NAME"
is "workspace: the later contents" "$(dev "$ID" 'cat ~/workspace/notes.txt')" v2
is "home: untouched" "$(dev "$ID" 'cat ~/.bh-verify-data')" v1

echo "== export and import =="
check "down" "$BH" down "$NAME"
mkdir -p "$WORK/out"
check "export" "$BH" export "$NAME" "$WORK/out"
EXPORTED=$(ls "$WORK/out"/*.tar.gz 2>/dev/null | head -1)
[ -n "$EXPORTED" ] && ok "the export is a file ($(du -h "$EXPORTED" | cut -f1))" || ng "no file in $WORK/out"
is "an export is not among the environment's snapshots" "$(snapshots_in "$ID")" 2
check "import under another name" "$BH" import "$EXPORTED" "$COPY"
COPY_ID=$(id_of "$COPY")
check "the copy gets a port of its own" "$BH" configure "$COPY" --port "$COPY_PORT"
check "up (nix starts from the image, init.sh runs)" "$BH" up "$COPY"
is "workspace came along" "$(dev "$COPY_ID" 'cat ~/workspace/notes.txt')" v2
is "home came along" "$(dev "$COPY_ID" 'cat ~/.bh-verify-data')" v1
refuse "the cache did not" dev "$COPY_ID" 'test -e ~/.cache/bh-verify'
[ -n "$(dev "$COPY_ID" 'nix --version')" ] && ok "nix works in the copy" || ng "nix does not work in the copy"
check "nix can run something" dev "$COPY_ID" 'nix eval --expr 1+1'
is "init.sh ran once more in the copy" "$(dev "$COPY_ID" 'wc -l < ~/.bh-verify-init | tr -d " "')" "$((INIT_BEFORE + 1))"
is "the home directory is the developer's" "$(dev "$COPY_ID" 'stat -c "%u:%g" ~')" "1000:1000"
check "ssh login to the copy" login "$COPY_PORT"
check "doctor finds nothing wrong with the copy" "$BH" doctor "$COPY"

echo "== archive and unarchive =="
check "up, to read the host key" "$BH" up "$NAME"
FP_BEFORE=$(docker exec "bulkhead-$ID-dev" ssh-keygen -lf /etc/bulkhead/hostkeys/ssh_host_ed25519_key.pub | awk '{print $2}')
check "down" "$BH" down "$NAME"
check "archive" "$BH" archive "$NAME" --yes
is "only the ssh identity is left of its volumes" "$(volumes_of "$ID")" hostkeys
is "no containers" "$(docker ps -aq --filter "name=bulkhead-$ID-" | wc -l | tr -d ' ')" 0
check "list says archived" bash -c "\"$BH\" list | grep -Eq '^$NAME +archived'"
refuse "up is refused" "$BH" up "$NAME"
is "…and created nothing" "$(volumes_of "$ID")" hostkeys
check "doctor is content" "$BH" doctor "$NAME"
refuse "its snapshot cannot be removed" "$BH" snapshot remove "$NAME" latest
check "unarchive" "$BH" unarchive "$NAME"
check "up (nix starts from the image, init.sh runs)" "$BH" up "$NAME"
is "workspace is back" "$(dev "$ID" 'cat ~/workspace/notes.txt')" v2
is "home is back" "$(dev "$ID" 'cat ~/.bh-verify-data')" v1
refuse "the cache is not" dev "$ID" 'test -e ~/.cache/bh-verify'
[ -n "$(dev "$ID" 'nix --version')" ] && ok "nix works again" || ng "nix does not work after unarchive"
is "init.sh ran once more" "$(dev "$ID" 'wc -l < ~/.bh-verify-init | tr -d " "')" "$((INIT_BEFORE + 1))"
FP_AFTER=$(docker exec "bulkhead-$ID-dev" ssh-keygen -lf /etc/bulkhead/hostkeys/ssh_host_ed25519_key.pub | awk '{print $2}')
if [ -n "$FP_BEFORE" ] && [ "$FP_BEFORE" = "$FP_AFTER" ]; then ok "the ssh host key is the same"; else ng "the ssh host key changed: $FP_BEFORE -> $FP_AFTER"; fi
check "ssh login" login "$PORT"
check "doctor finds nothing wrong" "$BH" doctor "$NAME"
