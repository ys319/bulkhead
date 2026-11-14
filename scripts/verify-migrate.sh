#!/usr/bin/env bash
# Checks that `bh migrate` took a real environment of the TypeScript version
# over without touching its data. It looks at the volumes itself, with
# docker, and takes nothing bh says for it.
#
#   scripts/verify-migrate.sh before <env>             # 1. while it is still the TypeScript version's
#   <bh> migrate <env>                                 # 2. the migration itself
#   scripts/verify-migrate.sh after <bh-binary> <env>  # 3. compare
#
# It only reads: each volume is mounted read-only into a one-off container
# without a network, made from the image the environment already runs. The
# one thing it writes is its own record of step 1, in a directory under
# ${TMPDIR:-/tmp} (BH_MIGRATE_CHECK_DIR to put it elsewhere). That record
# holds the names of the files in workspace and home — it stays on this
# machine, and the script prints counts, never names.
#
# What it compares:
#   - every volume is the same volume (same name, same creation time);
#   - workspace: every file, with its size and modification time;
#   - the ssh host keys, byte for byte;
#   - home: no path that was there is gone (caches and editor servers aside);
#   - nix: the same number of store entries;
#   - the old containers and network are gone, the new containers use the
#     same volumes at the same places, and the environment is in the state
#     it was in (running / stopped).
#
# Work in the environment between "before" and "after" shows up as a
# difference in workspace — keep your hands off it for those minutes.
set -uo pipefail
export MSYS_NO_PATHCONV=1
export LC_ALL=C

usage() {
  echo "usage: $0 before <env>" >&2
  echo "       $0 after <bh-binary> <env>" >&2
  exit 2
}

MODE=${1:-}
case "$MODE" in
  before) [ $# -eq 2 ] || usage; BH=; NAME=$2 ;;
  after) [ $# -eq 3 ] || usage; BH=$2; NAME=$3 ;;
  *) usage ;;
esac
case "$NAME" in
  "" | *[!a-z0-9-]* | -*) echo "\"$NAME\" is not an environment name" >&2; exit 2 ;;
esac
if [ -n "$BH" ]; then
  case "$BH" in /*) ;; *) BH=$PWD/$BH ;; esac
  [ -x "$BH" ] || { echo "$BH is not a program" >&2; exit 2; }
fi

ROOT=${BH_MIGRATE_CHECK_DIR:-${TMPDIR:-/tmp}/bh-migrate-check}
STATE=$ROOT/$NAME
FACTS=$STATE/facts

PROJECT=bulkhead-$NAME
KEYS="workspace home nix hostkeys github tokens"
OLD_HOLDER=$PROJECT-holder-1
OLD_DEV=$PROJECT-dev-1
OLD_NETWORK=${PROJECT}_default
NEW_HOLDER=$PROJECT-holder
NEW_DEV=$PROJECT-dev
NEW_NETWORK=$PROJECT

target_of() {
  case "$1" in
    workspace) echo /home/developer/workspace ;;
    home) echo /home/developer ;;
    nix) echo /nix ;;
    hostkeys) echo /etc/bulkhead/hostkeys ;;
    github) echo /etc/bulkhead/github ;;
    tokens) echo /etc/bulkhead/tokens ;;
  esac
}

PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); echo "  ok: $1"; }
ng() { FAIL=$((FAIL + 1)); echo "  NG: $1"; }
is() { # is <desc> <got> <want>
  if [ "$2" = "$3" ]; then ok "$1"; else ng "$1 — got \"$2\", want \"$3\""; fi
}

docker version >/dev/null 2>&1 || { echo "docker does not answer" >&2; exit 1; }

# state_of <container>: running / stopped / absent
state_of() {
  local status
  status=$(docker container inspect -f '{{.State.Status}}' "$1" 2>/dev/null) || { echo absent; return; }
  if [ "$status" = running ]; then echo running; else echo stopped; fi
}
created_of() { docker volume inspect -f '{{.CreatedAt}}' "$1" 2>/dev/null; }

# in_volume <volume> <script>: run the script with the volume at /v,
# read-only, as root without capabilities beyond reading (the files belong
# to several users), without a network.
in_volume() {
  docker run --rm --network none --entrypoint /bin/sh --user 0:0 \
    --cap-drop ALL --cap-add DAC_READ_SEARCH --security-opt no-new-privileges \
    -v "$1:/v:ro" "$IMAGE" -c "$2"
}

# What is read from each volume. The listings are sorted in the container,
# so both sides of a comparison come from the same tools.
LIST_ALL='cd /v && find . -xdev -printf "%y %s %T@ %p\n" | LC_ALL=C sort'
# home: paths only, and not what comes and goes by itself.
LIST_HOME='cd /v && find . -xdev \( -path ./.cache -o -path ./.vscode-server -o -path ./.local/state -o -path ./.npm/_logs \) -prune -o -print | LC_ALL=C sort'
SUM_FILES='cd /v && find . -xdev -type f -exec sha256sum {} + | LC_ALL=C sort -k 2'
COUNT_STORE='if [ -d /v/store ]; then ls -A /v/store | wc -l; else echo 0; fi'

# read_volumes <directory>: what the comparison needs, into the directory.
read_volumes() {
  local out=$1 key volume
  for key in $KEYS; do
    volume=${PROJECT}_$key
    created_of "$volume" >"$out/$key.created" || : >"$out/$key.created"
  done
  in_volume "${PROJECT}_workspace" "$LIST_ALL" >"$out/workspace.list" || return 1
  in_volume "${PROJECT}_home" "$LIST_HOME" >"$out/home.paths" || return 1
  in_volume "${PROJECT}_hostkeys" "$SUM_FILES" >"$out/hostkeys.sums" || return 1
  in_volume "${PROJECT}_nix" "$COUNT_STORE" | tr -d ' \r' >"$out/nix.count" || return 1
}

lines() { wc -l <"$1" | tr -d ' '; }

if [ "$MODE" = before ]; then
  if [ -e "$FACTS" ]; then
    echo "there is a record for $NAME already: $STATE" >&2
    echo "(taken $(sed -n 's/^taken=//p' "$FACTS")). If the environment has not been migrated since, move that" >&2
    echo "directory away and run this again; if it has, run: $0 after <bh-binary> $NAME" >&2
    exit 1
  fi
  holder=$(state_of "$OLD_HOLDER")
  dev=$(state_of "$OLD_DEV")
  if [ "$(state_of "$NEW_DEV")" != absent ] || [ "$(state_of "$NEW_HOLDER")" != absent ]; then
    echo "$NEW_HOLDER / $NEW_DEV exist: $NAME looks migrated already — \"before\" is for before that" >&2
    exit 1
  fi
  for key in workspace home nix hostkeys; do
    [ -n "$(created_of "${PROJECT}_$key")" ] || { echo "the volume ${PROJECT}_$key does not exist — is \"$NAME\" an environment of the TypeScript version?" >&2; exit 1; }
  done
  # The image the environment runs is on this machine and has the tools.
  IMAGE=$(docker container inspect -f '{{.Image}}' "$OLD_DEV" 2>/dev/null) || IMAGE=bulkhead-dev-base:latest
  docker image inspect "$IMAGE" >/dev/null 2>&1 || { echo "no image to read the volumes with ($IMAGE is not on this machine)" >&2; exit 1; }

  mkdir -p "$ROOT" && chmod 700 "$ROOT" && mkdir "$STATE.partial" || { echo "cannot make $STATE.partial (left from an interrupted run? remove it)" >&2; exit 1; }
  echo "Reading the volumes of $NAME (read-only) ..."
  if ! read_volumes "$STATE.partial"; then
    echo "reading the volumes failed; nothing was recorded" >&2
    rm -rf "$STATE.partial"
    exit 1
  fi
  {
    echo "taken=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "image=$IMAGE"
    echo "holder=$holder"
    echo "dev=$dev"
  } >"$STATE.partial/facts"
  mv "$STATE.partial" "$STATE"

  echo "Recorded in $STATE:"
  for key in $KEYS; do
    created=$(cat "$STATE/$key.created")
    echo "  ${PROJECT}_$key  ${created:-does not exist}"
  done
  echo "  workspace: $(lines "$STATE/workspace.list") entries; home: $(lines "$STATE/home.paths") paths;" \
    "nix store: $(cat "$STATE/nix.count") entries; host keys: $(lines "$STATE/hostkeys.sums") files"
  echo "  containers: holder $holder, dev $dev"
  echo
  echo "Next:  bh migrate $NAME      then:  $0 after <bh-binary> $NAME"
  exit 0
fi

# ---- after ----
[ -f "$FACTS" ] || { echo "no record for $NAME in $STATE — \"before\" was not run (or BH_MIGRATE_CHECK_DIR differs)" >&2; exit 1; }
IMAGE=$(sed -n 's/^image=//p' "$FACTS")
WAS_HOLDER=$(sed -n 's/^holder=//p' "$FACTS")
WAS_DEV=$(sed -n 's/^dev=//p' "$FACTS")
docker image inspect "$IMAGE" >/dev/null 2>&1 || IMAGE=$(docker container inspect -f '{{.Image}}' "$NEW_DEV" 2>/dev/null)
[ -n "$IMAGE" ] || { echo "no image to read the volumes with" >&2; exit 1; }

# Each run of "after" keeps what it read, next to the record of before.
NOW=$STATE/after-$(date -u +%Y%m%dT%H%M%SZ)
mkdir "$NOW" || exit 1
echo "Reading the volumes of $NAME (read-only) ..."
read_volumes "$NOW" || { echo "reading the volumes failed" >&2; exit 1; }

echo "== the volumes are the same volumes =="
for key in $KEYS; do
  before=$(cat "$STATE/$key.created")
  after=$(cat "$NOW/$key.created")
  if [ -z "$before" ]; then
    # It did not exist (the TypeScript version made these two on first use).
    echo "  --: ${PROJECT}_$key did not exist before (now: ${after:-does not exist})"
  elif [ -z "$after" ]; then
    ng "${PROJECT}_$key IS GONE"
  else
    is "${PROJECT}_$key was not made anew (created $before)" "$after" "$before"
  fi
done

echo "== their contents =="
if cmp -s "$STATE/workspace.list" "$NOW/workspace.list"; then
  ok "workspace: all $(lines "$NOW/workspace.list") entries as they were (path, size, modification time)"
else
  gone=$(comm -23 "$STATE/workspace.list" "$NOW/workspace.list" | wc -l | tr -d ' ')
  new=$(comm -13 "$STATE/workspace.list" "$NOW/workspace.list" | wc -l | tr -d ' ')
  ng "workspace differs: $gone entries of before are not there as they were, $new are new or changed (compare $STATE/workspace.list with $NOW/workspace.list; work done in between shows up here)"
fi
if cmp -s "$STATE/hostkeys.sums" "$NOW/hostkeys.sums" && [ -s "$NOW/hostkeys.sums" ]; then
  ok "ssh host keys: $(lines "$NOW/hostkeys.sums") files, byte for byte"
else
  ng "the ssh host keys differ (or there are none)"
fi
missing=$(comm -23 "$STATE/home.paths" "$NOW/home.paths" | wc -l | tr -d ' ')
if [ "$missing" = 0 ]; then
  ok "home: none of the $(lines "$STATE/home.paths") paths is gone"
else
  ng "home: $missing paths are gone (comm -23 $STATE/home.paths $NOW/home.paths lists them)"
fi
is "nix: the number of store entries" "$(cat "$NOW/nix.count")" "$(cat "$STATE/nix.count")"

echo "== the containers =="
is "the old holder ($OLD_HOLDER) is gone" "$(state_of "$OLD_HOLDER")" absent
is "the old dev ($OLD_DEV) is gone" "$(state_of "$OLD_DEV")" absent
if docker network inspect "$OLD_NETWORK" >/dev/null 2>&1; then ng "the old network $OLD_NETWORK is still there"; else ok "the old network is gone"; fi
want=stopped
if [ "$WAS_HOLDER" = running ] || [ "$WAS_DEV" = running ]; then want=running; fi
is "the new holder ($NEW_HOLDER) is $want, as the environment was" "$(state_of "$NEW_HOLDER")" "$want"
is "the new dev ($NEW_DEV) is $want, as the environment was" "$(state_of "$NEW_DEV")" "$want"
mounts=$(docker container inspect -f '{{range .Mounts}}{{.Name}} {{.Destination}}{{"\n"}}{{end}}' "$NEW_DEV" 2>/dev/null)
for key in $KEYS; do
  if printf '%s\n' "$mounts" | grep -qx "${PROJECT}_$key $(target_of "$key")"; then
    ok "dev uses ${PROJECT}_$key at $(target_of "$key")"
  else
    ng "dev does not use ${PROJECT}_$key at $(target_of "$key")"
  fi
done
if docker network inspect "$NEW_NETWORK" >/dev/null 2>&1; then ok "the new network $NEW_NETWORK"; else ng "the new network $NEW_NETWORK is missing"; fi

echo "== bh =="
if "$BH" status "$NAME" >"$NOW/status" 2>&1; then
  ok "bh status $NAME answers"
  is "bh kept the name as the id" "$(awk '$1 == "id:" {print $2}' "$NOW/status")" "$NAME"
else
  ng "bh status $NAME fails"
  sed 's/^/      | /' "$NOW/status"
fi
if [ "$want" = running ]; then
  if "$BH" ssh "$NAME" true >"$NOW/ssh" 2>&1; then
    ok "ssh login (bh ssh $NAME true)"
  else
    ng "ssh login fails"
    sed 's/^/      | /' "$NOW/ssh"
  fi
fi

echo
echo "passed: $PASS, failed: $FAIL"
if [ "$FAIL" -eq 0 ]; then
  echo "The record of before is in $STATE — remove it when you no longer want it."
  [ "$want" = running ] && echo "The boundary of the migrated environment: scripts/verify.sh <bh-binary> $NAME" \
    "(it restarts the environment and leaves a marker file in workspace for a moment)"
fi
[ "$FAIL" -eq 0 ]
