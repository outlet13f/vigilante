#!/usr/bin/env bash
# Builds the release artifacts into dist/<version>/. Used by
# .github/workflows/release.yml and runnable locally (Linux, macOS, Git Bash).
#
#   scripts/release.sh build  VERSION   binaries (full + minimal), SBOMs,
#                                       rpm/deb, Helm chart
#   scripts/release.sh bundle VERSION   air-gapped install bundles (one per
#                                       linux arch) and SHA256SUMS over
#                                       everything; picks up container image
#                                       archives from dist/VERSION/images/
#   scripts/release.sh sign   VERSION   cosign signature of SHA256SUMS with
#                                       the key in $COSIGN_PRIVATE_KEY
#                                       (password in $COSIGN_PASSWORD)
#   scripts/release.sh verify VERSION [PUBKEY]
#                                       signature (openssl only) and checksums;
#                                       PUBKEY defaults to packaging/cosign.pub
#
# VERSION is SemVer without the leading v (1.0.0, 1.1.0-rc.1). The signature
# covers SHA256SUMS, which covers every artifact (bundles included).
set -euo pipefail

usage="usage: scripts/release.sh build|bundle|sign|verify VERSION"
cmd=${1:?$usage}
VERSION=${2:?$usage}
VERSION=${VERSION#v}
if ! [[ $VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
    echo "version $VERSION is not SemVer (MAJOR.MINOR.PATCH[-pre])" >&2
    exit 1
fi

cd "$(dirname "$0")/.."
OUT=dist/$VERSION
LINUX_ARCHES=(amd64 arm64 ppc64le)

# Pinned tool versions (go run fetches them; no global install needed).
NFPM=github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.41.1
CYCLONEDX=github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.9.0

SHA256=(sha256sum)
command -v sha256sum >/dev/null || SHA256=(shasum -a 256)

build() {
    rm -rf "$OUT"
    mkdir -p "$OUT"
    local commit date ldflags
    commit=$(git rev-parse --short=12 HEAD)
    # The commit time, not the wall clock: rebuilding a tag gives the same bytes.
    date=$(git log -1 --format=%cI)
    ldflags="-s -w -X main.version=$VERSION -X main.commit=$commit -X main.date=$date"

    local targets=() t
    for t in "${LINUX_ARCHES[@]}"; do targets+=("linux/$t"); done
    targets+=(windows/amd64)
    for t in "${targets[@]}"; do
        local os=${t%/*} arch=${t#*/}
        for flavor in full minimal; do
            local name="vigilante_${VERSION}_${os}_${arch}" tags=""
            if [ "$flavor" = minimal ]; then name+="-minimal"; tags=minimal; fi
            [ "$os" = windows ] && name+=".exe"
            CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -tags "$tags" -ldflags "$ldflags" -o "$OUT/$name" ./cmd/vigilante
            # SBOM from the module list embedded in the binary: exactly what shipped.
            go run "$CYCLONEDX" bin -json -version "$VERSION" -output "$OUT/$name.cdx.json" "$OUT/$name"
            echo "built $name"
        done
    done

    # rpm and deb (full build) for each linux arch.
    for arch in "${LINUX_ARCHES[@]}"; do
        mkdir -p dist/.pkg
        cp "$OUT/vigilante_${VERSION}_linux_${arch}" dist/.pkg/vigilante
        for fmt in deb rpm; do
            VERSION=$VERSION ARCH=$arch go run "$NFPM" package --config packaging/nfpm.yaml --packager "$fmt" --target "$OUT/"
        done
    done
    rm -rf dist/.pkg

    # Helm chart with the release version.
    if command -v helm >/dev/null; then
        # The default config has no auth, which the chart refuses to render.
        helm lint deploy/helm/vigilante --set auth.allowAnonymous=true
        helm package deploy/helm/vigilante --version "$VERSION" --app-version "$VERSION" --destination "$OUT"
    else
        echo "helm not found: skipping the chart package" >&2
    fi
    ls -l "$OUT"
}

bundle() {
    [ -d "$OUT" ] || { echo "run: scripts/release.sh build $VERSION" >&2; exit 1; }
    rm -f "$OUT"/*_airgap_*.tar.gz "$OUT/SHA256SUMS"
    for arch in "${LINUX_ARCHES[@]}"; do
        local name="vigilante_${VERSION}_airgap_linux_${arch}"
        local dir
        dir=$(mktemp -d)
        local b="$dir/$name"
        mkdir -p "$b/bin" "$b/packages" "$b/sbom" "$b/chart" "$b/image" "$b/docs"
        cp "$OUT/vigilante_${VERSION}_linux_${arch}" "$OUT/vigilante_${VERSION}_linux_${arch}-minimal" "$b/bin/"
        cp "$OUT/vigilante_${VERSION}_linux_${arch}".cdx.json "$OUT/vigilante_${VERSION}_linux_${arch}-minimal".cdx.json "$b/sbom/"
        local deb_arch=$arch
        [ "$arch" = ppc64le ] && deb_arch=ppc64el
        local rpm_arch=$arch
        [ "$arch" = amd64 ] && rpm_arch=x86_64
        [ "$arch" = arm64 ] && rpm_arch=aarch64
        cp "$OUT"/*_"$deb_arch".deb "$OUT"/*."$rpm_arch".rpm "$b/packages/"
        cp "$OUT"/vigilante-"$VERSION".tgz "$b/chart/" 2>/dev/null || true
        cp "$OUT"/images/*_linux_"$arch".tar "$b/image/" 2>/dev/null || true
        cp README.md "$b/"
        cp docs/*.md "$b/docs/"
        cp api/openapi.yaml "$b/docs/"
        cp -r examples/config "$b/docs/examples"
        cp packaging/install-airgap.sh "$b/install.sh"
        (cd "$b" && find . -type f ! -name SHA256SUMS | sort | sed 's|^\./||' | xargs "${SHA256[@]}" > SHA256SUMS)
        tar -C "$dir" -czf "$OUT/$name.tar.gz" "$name"
        rm -rf "$dir"
        echo "bundled $name.tar.gz"
    done
    (cd "$OUT" && find . -maxdepth 1 -type f ! -name SHA256SUMS ! -name 'SHA256SUMS.*' | sort | sed 's|^\./||' | xargs "${SHA256[@]}" > SHA256SUMS)
    echo "wrote $OUT/SHA256SUMS ($(wc -l < "$OUT/SHA256SUMS") files)"
}

sign() {
    : "${COSIGN_PRIVATE_KEY:?set COSIGN_PRIVATE_KEY (cosign generate-key-pair) and COSIGN_PASSWORD}"
    [ -f "$OUT/SHA256SUMS" ] || { echo "run: scripts/release.sh bundle $VERSION" >&2; exit 1; }
    # No transparency-log upload: releases are internal and verified offline.
    cosign sign-blob --yes --key env://COSIGN_PRIVATE_KEY --tlog-upload=false \
        --output-signature "$OUT/SHA256SUMS.sig" "$OUT/SHA256SUMS"
    cosign public-key --key env://COSIGN_PRIVATE_KEY --outfile "$OUT/cosign.pub"
    if [ -f packaging/cosign.pub ] && ! cmp -s <(openssl pkey -pubin -in packaging/cosign.pub -outform DER) \
        <(openssl pkey -pubin -in "$OUT/cosign.pub" -outform DER); then
        echo "the signing key does not match packaging/cosign.pub: refusing to sign with an unknown key" >&2
        exit 1
    fi
    verify "$OUT/cosign.pub"
}

# verify checks what an operator checks before installing: the signature of
# SHA256SUMS with the published key (openssl, no cosign needed), then every
# file against SHA256SUMS.
verify() {
    local pub=${1:-packaging/cosign.pub}
    [ -f "$pub" ] || { echo "public key $pub not found" >&2; exit 1; }
    local der
    der=$(mktemp)
    openssl base64 -d -A -in "$OUT/SHA256SUMS.sig" -out "$der"
    openssl dgst -sha256 -verify "$pub" -signature "$der" "$OUT/SHA256SUMS"
    rm -f "$der"
    (cd "$OUT" && "${SHA256[@]}" -c --quiet SHA256SUMS)
    echo "all $(wc -l < "$OUT/SHA256SUMS") files match the signed SHA256SUMS"
}

case $cmd in
build) build ;;
bundle) bundle ;;
sign) sign ;;
verify) verify "${3:-}" ;;
*) echo "unknown command $cmd; $usage" >&2; exit 1 ;;
esac
