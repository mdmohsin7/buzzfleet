#!/usr/bin/env sh
# buzzfleet installer.
#
#   curl -fsSL https://raw.githubusercontent.com/mdmohsin7/buzzfleet/main/install.sh | sh
#
# Downloads one static binary and lets it install itself. There is nothing to
# clone and no runtime to add: the systemd units are written by the binary, and
# the agent launcher is a subcommand rather than a script that has to live in
# the right place.
#
# Honours: BUZZFLEET_VERSION (default: latest), BUZZFLEET_HOME (default:
# ~/.buzzfleet), BUZZFLEET_BIN_DIR (default: ~/.local/bin).
set -eu

REPO="mdmohsin7/buzzfleet"
VERSION="${BUZZFLEET_VERSION:-latest}"
FLEET_HOME="${BUZZFLEET_HOME:-$HOME/.buzzfleet}"
BIN_DIR="${BUZZFLEET_BIN_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  *) die "unsupported OS $(uname -s) — buzzfleet supervises agents with systemd, so Linux is the target" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

ASSET="buzzfleet-${OS}-${ARCH}"
if [ "$VERSION" = latest ]; then
  URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"
else
  URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"
fi

command -v curl >/dev/null || die "curl is required"

say "buzzfleet: fetching ${ASSET} (${VERSION})"
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
# Fail loudly on a 404 rather than saving an error page and chmod +x-ing it.
curl -fsSL "$URL" -o "$TMP" || die "could not download $URL
If the repository is private, releases are not publicly downloadable — build
from source instead: git clone https://github.com/${REPO} && cd buzzfleet && make build"

# A downloaded binary that is not a binary is the classic silent failure here.
head -c 4 "$TMP" | grep -q 'ELF' || [ "$OS" = darwin ] || die "downloaded file is not an executable — check $URL"

mkdir -p "$BIN_DIR" "$FLEET_HOME"
install -m 755 "$TMP" "$BIN_DIR/buzzfleet"
say "buzzfleet: installed $BIN_DIR/buzzfleet"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) say "buzzfleet: add $BIN_DIR to PATH, e.g. echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.profile" ;;
esac

# The binary writes its own units and reports what this host is missing.
FLEET_HOME="$FLEET_HOME" "$BIN_DIR/buzzfleet" install
