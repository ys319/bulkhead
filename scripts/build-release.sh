#!/bin/sh
# Builds the files that are released: one binary per system, and SHA256SUMS.
#
#   scripts/build-release.sh [<version>]     # default: 0.0.0-dev
#
# The release workflow runs exactly this, so that a release can be made
# again on a desk and compared with what was published. For that the Go
# toolchain has to be the one the workflow uses: the version in go.mod's
# toolchain line, as go.dev ships it. The Go of this repository's devShell is
# nixpkgs' build, which writes /nix/store paths into binaries for Linux and
# macOS — fine for trying things out, not the same bytes.
#
# Nothing is read from the git checkout but the commit (BH_COMMIT overrides
# it), and the build does not depend on what else is in the working tree.
#
# Environment: BH_DIST (where to write, default dist), BH_COMMIT.
set -eu

cd "$(dirname "$0")/.."

version=${1:-0.0.0-dev}
case "$version" in
v*) echo "build-release: the version goes without the v: ${version#v}" >&2; exit 2 ;;
esac
commit=${BH_COMMIT:-$(git rev-parse HEAD | cut -c1-7)}
out=${BH_DIST:-dist}

case "$(go env GOROOT)" in
/nix/store/*)
	echo "build-release: note: this Go is nixpkgs' build; the binaries will not match released ones byte for byte" >&2
	;;
esac

mkdir -p "$out"
names=
# Windows first: it is the system with no other way to get bh.
for target in windows/amd64 windows/arm64 darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
	os=${target%/*}
	arch=${target#*/}
	name=bh-$os-$arch
	if [ "$os" = windows ]; then
		name=$name.exe
	fi
	echo "build-release: $name" >&2
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build \
		-trimpath -buildvcs=false \
		-ldflags "-X main.version=$version -X main.commit=$commit" \
		-o "$out/$name" ./cmd/bh
	names="$names $name"
done

cd "$out"
if command -v sha256sum >/dev/null 2>&1; then
	# shellcheck disable=SC2086 # the names have no spaces; they are a list
	sha256sum $names >SHA256SUMS
else
	# shellcheck disable=SC2086
	shasum -a 256 $names >SHA256SUMS
fi
echo "build-release: $(go version | cut -d' ' -f3), version $version, commit $commit" >&2
cat SHA256SUMS
