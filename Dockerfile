# syntax=docker/dockerfile:1

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
FROM gcr.io/distroless/static-debian12:nonroot

# Snapshots are data, not code: mount them at /data/fixtures.
#   docker run -v $PWD/fixtures:/data/fixtures ghcr.io/nrf24l01/proxy_fallback \
#     serve -service grafana -port 8080 -listen 0.0.0.0
WORKDIR /data

COPY --from=build /out/srvimit /usr/local/bin/srvimit
COPY --from=build /out/catalogup /usr/local/bin/catalogup
COPY --chown=65532:65532 catalog /data/catalog

USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/srvimit"]
CMD ["list"]