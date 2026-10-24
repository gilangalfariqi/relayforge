# syntax=docker/dockerfile:1.7

FROM golang:1.27.3-alpine AS build

WORKDIR /src

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w \
          -X github.com/relayforge/relayforge/api/internal/version.Version=${VERSION} \
          -X github.com/relayforge/relayforge/api/internal/version.Commit=${COMMIT} \
          -X github.com/relayforge/relayforge/api/internal/version.BuildTime=${BUILD_TIME}" \
        -o /out/api ./cmd/api \
    && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/migrate ./cmd/migrate \
    && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/receiver ./cmd/receiver

# The runtime is intentionally minimal: no shell, no package manager, no
# compiler. It runs as an unprivileged numeric user. The api and receiver
# targets share the same binaries but differ only in their entrypoint, so the
# published images are self-describing.
FROM scratch AS runtime

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/api /api
COPY --from=build /out/migrate /migrate
COPY --from=build /out/receiver /receiver
COPY --from=build /src/migrations /migrations

USER 65532:65532
EXPOSE 8080 9090

FROM runtime AS receiver
EXPOSE 8090
ENTRYPOINT ["/receiver"]

# Keep api last so an untargeted build keeps the API entrypoint.
FROM runtime AS api
ENTRYPOINT ["/api"]
