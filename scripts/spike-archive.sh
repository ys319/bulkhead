#!/usr/bin/env bash
# Spike for the snapshot design (rewrite-spec-draft §8): can a volume be
# read and written as a tar stream through the engine's archive API
# (`docker cp`), on a container that is never started — no tar process, no
# helper script inside? And does what comes back equal what went in?
#
#   scripts/spike-archive.sh
#
# It makes two volumes and a few containers of its own, all named
# bh-spike-<pid>-*, and removes exactly those at the end. It touches no
# other volume. Needs the dev base image (any environment that was started
# once has built it).
set -uo pipefail

TAG=bh-spike-$$
SRC=$TAG-src
DST=$TAG-dst
HOME_VOLUME=$TAG-home
WORK=$(mktemp -d) && [ -d "$WORK" ] || { echo "cannot make a work directory" >&2; exit 1; }
BUNDLE=$WORK/volume.tar

IMAGE=$(docker image ls bulkhead-dev-base --format '{{.Repository}}:{{.Tag}}' | head -1)
if [ -z "$IMAGE" ]; then
  echo "no bulkhead-dev-base image here: start any environment once (bh up) and run this again" >&2
  exit 1
fi

cleanup() {
  docker rm -f "$TAG-fill" "$TAG-read" "$TAG-write" "$TAG-check" "$TAG-fresh" >/dev/null 2>&1
  docker volume rm "$SRC" "$DST" "$HOME_VOLUME" >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

say() { echo "  $1"; }
# In a container that has the volume and nothing else.
inside() { # inside <name> <volume> <script>
  docker run --rm --name "$1" --network none --user 0:0 --entrypoint sh -v "$2:/data" "$IMAGE" -c "$3"
}

echo "image: $IMAGE"
echo "== fill a volume =="
inside "$TAG-fill" "$SRC" '
  set -e
  mkdir -p /data/dir/private && chmod 700 /data/dir/private
  echo secret > /data/dir/private/token && chmod 600 /data/dir/private/token
  echo "with space" > "/data/dir/a file"
  ln -s /nix/store/does-not-exist /data/dir/dangling
  echo linked > /data/dir/one && ln /data/dir/one /data/dir/two
  chown -R 1000:1000 /data/dir
  echo root > /data/root-owned && chmod 644 /data/root-owned
  dd if=/dev/urandom of=/data/big bs=1M count=300 2>/dev/null
  chown 1000:1000 /data/big
' && say "ok: filled (300MB of random data and a few special cases)" || { say "NG: could not fill the volume"; exit 1; }
SUM_BEFORE=$(inside "$TAG-check" "$SRC" 'sha256sum /data/big | cut -d" " -f1')

echo "== read it through a container that never starts =="
docker create --name "$TAG-read" --network none -v "$SRC:/data" "$IMAGE" >/dev/null
SECONDS=0
if docker cp "$TAG-read:/data/." - >"$BUNDLE"; then
  say "ok: docker cp from a created container reads the volume ($(du -h "$BUNDLE" | cut -f1) in ${SECONDS}s)"
else
  say "NG: docker cp cannot read the volume of a container that was never started"
  exit 1
fi
say "entries: $(tar -tf "$BUNDLE" | wc -l | tr -d ' ') — the first few:"
tar -tf "$BUNDLE" | head -5 | sed 's/^/      | /'

echo "== write it into an empty volume the same way =="
docker volume create "$DST" >/dev/null
docker create --name "$TAG-write" --network none -v "$DST:/data" "$IMAGE" >/dev/null
SECONDS=0
if docker cp -a - "$TAG-write:/data" <"$BUNDLE"; then
  say "ok: docker cp into a created container writes the volume (${SECONDS}s)"
else
  say "NG: docker cp cannot write the volume of a container that was never started"
  exit 1
fi

echo "== what came back =="
REPORT=$(inside "$TAG-check" "$DST" '
  cd /data
  echo "token $(stat -c "%u:%g %a" dir/private/token 2>&1)"
  echo "private $(stat -c "%u:%g %a" dir/private 2>&1)"
  echo "rootfile $(stat -c "%u:%g %a" root-owned 2>&1)"
  echo "space $(cat "dir/a file" 2>&1)"
  echo "dangling $(readlink dir/dangling 2>&1)"
  echo "hardlink $([ "$(stat -c %i dir/one)" = "$(stat -c %i dir/two)" ] && echo same || echo different)"
  echo "big $(sha256sum big | cut -d" " -f1)"
')
expect() { # expect <key> <value> <description>
  got=$(printf '%s\n' "$REPORT" | awk -v k="$1" '$1 == k { sub(/^[^ ]+ /, ""); print }')
  if [ "$got" = "$2" ]; then say "ok: $3"; else say "NG: $3 — got \"$got\", want \"$2\""; fi
}
expect token "1000:1000 600" "owner and mode of a private file"
expect private "1000:1000 700" "owner and mode of a private directory"
expect rootfile "0:0 644" "a file of root stays root's"
expect space "with space" "a name with a space"
expect dangling "/nix/store/does-not-exist" "a symbolic link that leads nowhere"
expect hardlink "same" "two names of one file are still one file"
expect big "$SUM_BEFORE" "300MB come back bit for bit"

echo "== does an empty home volume get the image's home before the container starts? =="
docker volume create "$HOME_VOLUME" >/dev/null
docker create --name "$TAG-fresh" --network none -v "$HOME_VOLUME:/home/developer" "$IMAGE" >/dev/null
if docker cp "$TAG-fresh:/home/developer/." - 2>/dev/null | tar -tf - 2>/dev/null | grep -q '\.config/nix/nix\.conf$'; then
  say "yes: created is enough — the volume already has the image's home"
else
  say "no: the volume is still empty after create (it is filled at the first start)"
fi

echo
echo "done — the volumes and containers named $TAG-* are removed now"
