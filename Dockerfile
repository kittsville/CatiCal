# syntax=docker/dockerfile:1

FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/catical ./cmd/catical

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/catical /catical
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/catical"]
