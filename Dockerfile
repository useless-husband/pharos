# syntax=docker/dockerfile:1
# Cross-compiles on the build machine's architecture; no emulation needed.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/pharos ./cmd/pharos \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="Pharos" \
      org.opencontainers.image.description="Uptime monitoring and status pages in a single binary" \
      org.opencontainers.image.source="https://github.com/useless-husband/pharos" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/pharos /usr/local/bin/pharos
COPY --from=build --chown=65532:65532 /out/data /data
ENV PHAROS_CONFIG=/etc/pharos/pharos.yaml \
    PHAROS_STORAGE_PATH=/data/pharos.db
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/pharos", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/pharos"]
CMD ["run"]
