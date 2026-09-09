# syntax=docker/dockerfile:1

# ---- Build stage ----
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=00000000-000000

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
  -ldflags="-X mediacruncher/internal/observability.BuildVersion=${BUILD_VERSION} -X mediacruncher/internal/observability.BuildCommit=${BUILD_COMMIT} -X mediacruncher/internal/observability.BuildTime=${BUILD_TIME}" \
  -o /app/mediacruncher \
  ./cmd/main.go

# ---- Runtime stage ----
FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ffmpeg \
      ca-certificates \
      tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && apt-get clean

RUN useradd --uid 1000 --create-home mediacruncher && \
    mkdir -p /app/data /app/config && \
    chown -R mediacruncher:mediacruncher /app

COPY --from=builder /app/mediacruncher /app/mediacruncher

WORKDIR /app

USER mediacruncher

ENTRYPOINT ["/app/mediacruncher"]
