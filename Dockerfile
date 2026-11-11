# ---- build -------------------------------------------------------------------
FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/srvimit ./cmd/srvimit \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/catalogup ./cmd/catalogup

# ---- runtime -----------------------------------------------------------------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 65532 -s /sbin/nologin srvimit

# Snapshots ship inside the image: `docker compose up -d grafana` works with no
# volume mounts. Rebuild the image (docker compose build) after re-recording.

# Snapshots are data, not code: mount them at /data/fixtures.
#   docker compose up -d grafana
#   docker run -v $PWD/fixtures:/data/fixtures:ro ghcr.io/nrf24l01/proxy_fallback \
#     serve -service grafana -port 8080 -listen 0.0.0.0
COPY --from=build /out/srvimit /usr/local/bin/srvimit
COPY --from=build /out/catalogup /usr/local/bin/catalogup
COPY --chown=65532:65532 catalog /data/catalog
COPY --chown=65532:65532 fixtures /data/fixtures

# Everything inside the container resolves snapshots without any flags.
ENV SRVIMIT_FIXTURES=/data/fixtures

WORKDIR /data
USER 65532:65532
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD srvimit list -fixtures /data/fixtures >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/srvimit"]
CMD ["list"]