# Dev container: Debian + non-root sshd. A glibc base is a hard requirement of
# VS Code Remote SSH — DECIDED: docs/decisions/0004-base-image-debian.md
FROM debian:trixie-slim

# curl + ca-certificates: VS Code Remote SSH downloads its server on connect;
# xz-utils: the Nix installer unpacks .tar.xz; git: baseline dev tooling;
# libatomic1: the official Node.js Linux binaries (nvm / fnm / volta, Node 26)
# link against libatomic.so.1, which slim Debian does not ship.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        openssh-server ca-certificates curl git zsh xz-utils libatomic1 \
    && rm -rf /var/lib/apt/lists/*

# grml's zsh config, system-wide (replaces Debian's zsh-common zshrc). Not
# packaged in trixie, so fetch the release tag and pin by checksum — a bad
# download fails the build, never the runtime. Local customisation goes to
# /etc/zsh/zshrc.local or ~/.zshrc.local (grml sources both at the end).
ARG GRML_ZSHRC_VERSION=v0.19.28
ARG GRML_ZSHRC_SHA256=2224349bbefc4b7013e6b01423cefa3813c4a8d13220051a5608a31339d7aeb7
RUN curl -fsSL "https://raw.githubusercontent.com/grml/grml-etc-core/${GRML_ZSHRC_VERSION}/etc/zsh/zshrc" \
        -o /etc/zsh/zshrc \
    && echo "${GRML_ZSHRC_SHA256}  /etc/zsh/zshrc" | sha256sum -c -

RUN useradd --create-home --uid 1000 --shell /usr/bin/zsh developer

# Single-user Nix, baked into the image: /nix and the home volume both
# initialize from this layer on first use, so a volume reset returns exactly
# here — DECIDED: docs/decisions/0009-bake-permanent-setup-into-image.md
RUN mkdir -m 0755 /nix && chown developer:developer /nix

USER developer
WORKDIR /home/developer

# --no-channel-add: nixpkgs comes through the flake registry on demand (into
# the nix volume, GC-able). A baked legacy channel would add ~515MB of dead
# weight and a second, stale nixpkgs copy next to the flake one.
ARG NIX_VERSION=2.35.1
RUN export USER=developer HOME=/home/developer NIX_INSTALLER_NO_MODIFY_PROFILE=1 \
    && curl -fsSL "https://releases.nixos.org/nix/nix-${NIX_VERSION}/install" -o /tmp/install-nix \
    && sh /tmp/install-nix --no-daemon --no-channel-add \
    && rm /tmp/install-nix

RUN mkdir -p /home/developer/.config/nix /home/developer/workspace \
    && printf 'experimental-features = nix-command flakes\n' > /home/developer/.config/nix/nix.conf

COPY --chown=developer:developer zshenv /home/developer/.zshenv

# sshd bits live below the heavy Nix layer so editing them stays cheap.
USER root
COPY sshd_config /etc/bulkhead/sshd_config
COPY entrypoint.sh /usr/local/bin/bulkhead-dev-entrypoint
RUN chmod 755 /usr/local/bin/bulkhead-dev-entrypoint

# bh-guest: every in-container step of bulkhead (init, credentials, status).
# Root-owned so the dev user cannot rewrite it — ADR 0035. Wired system-wide
# (not in ~/.zshenv / ~/.gitconfig, which live in the home volume and would
# never see an image update): git asks it for github.com over HTTPS before
# the user's own helpers, and every zsh evals its exports (GH_TOKEN) — both
# are no-ops while no token is injected (ADR 0036).
COPY bh-guest /usr/local/bin/bh-guest
RUN chmod 755 /usr/local/bin/bh-guest \
    && git config --system 'credential.https://github.com.helper' '/usr/local/bin/bh-guest git-helper' \
    && printf '\n# bulkhead (ADR 0035): exports injected by the host, e.g. GH_TOKEN\neval "$(/usr/local/bin/bh-guest env)"\n' >> /etc/zsh/zshenv

# Host-key volume mount point, baked with developer ownership: a named volume
# inherits the image directory's owner on first use (root-owned would leave
# the unprivileged entrypoint unable to write) — ADR 0022.
RUN install -d -o developer -g developer /etc/bulkhead/hostkeys

# No sudo and no setuid path back to root: privileged maintenance goes through
# an image rebuild or `docker exec --privileged` (ADR 0007).
USER developer

ENTRYPOINT ["/usr/local/bin/bulkhead-dev-entrypoint"]
