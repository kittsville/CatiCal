# syntax=docker/dockerfile:1

FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/catical ./cmd/catical

# Slim, not distroless: Coolify healthchecks exec /bin/sh then curl (or wget) against /healthz.
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl wget \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/catical /catical
USER nobody
EXPOSE 8080
ENTRYPOINT ["/catical"]
