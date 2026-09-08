# syntax=docker/dockerfile:1

# ---- Build stage ----
FROM golang:1.25-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=00000000-000000

RUN go build \
  -ldflags="-X mediacruncher/internal/observability.BuildVersion=${BUILD_VERSION} -X mediacruncher/internal/observability.BuildCommit=${BUILD_COMMIT} -X mediacruncher/internal/observability.BuildTime=${BUILD_TIME}" \
  -o /app/mediacruncher \
  ./cmd/main.go

# ---- Runtime stage ----
FROM debian:stable-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ffmpeg \
      libvmaf0 \
      libvmaf-dev \
    && rm -rf /var/lib/apt/lists/* \
    && apt-get clean

RUN useradd --uid 1000 --create-home mediacruncher

COPY --from=builder /app/mediacruncher /app/mediacruncher

WORKDIR /app

USER mediacruncher

ENTRYPOINT ["/app/mediacruncher"]
