# Server image from the release binaries (scripts/release.sh build runs
# first), so the image holds exactly the signed binary:
#   docker buildx build -f packaging/container/Dockerfile \
#     --build-arg VERSION=1.0.0 --platform linux/amd64,linux/arm64,linux/ppc64le .
# Static binary on distroless: no shell, no package manager, runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION
ARG TARGETARCH
LABEL org.opencontainers.image.title="vigilante" \
      org.opencontainers.image.description="Unified rollback orchestrator" \
      org.opencontainers.image.source="https://github.com/outlet13f/vigilante" \
      org.opencontainers.image.version="${VERSION}"

COPY dist/${VERSION}/vigilante_${VERSION}_linux_${TARGETARCH} /usr/bin/vigilante

# The file state store (single node) writes here; mount a volume.
WORKDIR /var/lib/vigilante
USER nonroot:nonroot
EXPOSE 8088
ENTRYPOINT ["/usr/bin/vigilante"]
CMD ["server", "-c", "/etc/vigilante/vigilante.yaml"]
