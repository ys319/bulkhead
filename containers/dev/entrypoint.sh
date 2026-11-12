#!/bin/sh
# Dev container entrypoint: prepare the ssh skeleton inside the (possibly
# fresh) home volume, then run sshd in the foreground as the unprivileged user.
set -eu

log() { echo "[dev] $*"; }

HOME_DIR=/home/developer
# Host keys live in their own tiny volume so the ssh identity survives not
# only a recreate but also reset / restore, which replace the home volume —
# ADR 0022 (the mount point is baked into the image with developer ownership).
KEY_DIR=/etc/bulkhead/hostkeys

install -d -m 700 "$HOME_DIR/.ssh" "$KEY_DIR"

if [ ! -f "$KEY_DIR/ssh_host_ed25519_key" ]; then
  ssh-keygen -q -t ed25519 -N '' -f "$KEY_DIR/ssh_host_ed25519_key"
  log "generated new ssh host key"
fi

# The management tool injects the user's public key via the environment; a key
# already present in the volume is kept when the variable is unset.
if [ -n "${SSH_AUTHORIZED_KEYS:-}" ]; then
  printf '%s\n' "$SSH_AUTHORIZED_KEYS" > "$HOME_DIR/.ssh/authorized_keys"
  chmod 600 "$HOME_DIR/.ssh/authorized_keys"
elif [ ! -f "$HOME_DIR/.ssh/authorized_keys" ]; then
  log "WARNING: no authorized_keys and SSH_AUTHORIZED_KEYS is empty — ssh login will fail"
fi

# One-time user init (ADR 0017): runs on the first boot of a fresh home volume
# (create / reset), before sshd — so "ssh works" implies "init finished".
# The procedure itself lives in bh-guest, shared with `bh init-run`
# (ADR 0035). set -e makes a failing init abort the boot (fail loudly); the
# marker is written only on success, so the next start retries.
/usr/local/bin/bh-guest init --once

exec /usr/sbin/sshd -D -e -f /etc/bulkhead/sshd_config
