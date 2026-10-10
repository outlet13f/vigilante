#!/bin/sh
# Air-gapped install of Vigilante from this bundle. Run as root inside the
# extracted directory:
#
#   ./install.sh            verify, then install the rpm/deb (full build)
#   ./install.sh --minimal  install the minimal binary to /usr/bin instead
#   ./install.sh --verify   only verify the bundle
#
# The container image (image/*.tar) and Helm chart (chart/*.tgz) are for
# Kubernetes: load the image into your registry and see docs/07-install.md.
set -eu
cd "$(dirname "$0")"

mode=package
case "${1:-}" in
--minimal) mode=minimal ;;
--verify) mode=verify ;;
"") ;;
*) echo "usage: $0 [--minimal|--verify]" >&2; exit 2 ;;
esac

echo "== verifying file checksums"
if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c --quiet SHA256SUMS
else
    shasum -a 256 -c --quiet SHA256SUMS
fi
echo "   ok"
echo "   Also check that this bundle's SHA256 matches the signed SHA256SUMS of the"
echo "   release (cosign verify-blob or openssl, see docs/07-install.md)."

[ "$mode" = verify ] && exit 0
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }

if [ "$mode" = minimal ]; then
    install -m 0755 bin/vigilante_*-minimal /usr/bin/vigilante
    echo "installed /usr/bin/vigilante (minimal build, no systemd units or config)"
    /usr/bin/vigilante version
    exit 0
fi

if command -v rpm >/dev/null 2>&1 && ! command -v dpkg >/dev/null 2>&1; then
    if rpm -q vigilante >/dev/null 2>&1; then rpm -Uvh packages/*.rpm; else rpm -ivh packages/*.rpm; fi
elif command -v dpkg >/dev/null 2>&1; then
    dpkg -i packages/*.deb
else
    echo "neither rpm nor dpkg found: copy bin/vigilante_* to /usr/bin/vigilante by hand" >&2
    exit 1
fi
/usr/bin/vigilante version
