# ==========================================
# Stage 1: Build pure Go zero-CGO binary
# ==========================================
FROM golang:alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.Version=1.0.0" -o /mediacruncher ./cmd/mediacruncher

# ==========================================
# Stage 2: Multi-GPU accelerated FFmpeg runtime (NVIDIA NVENC, Intel QSV, VAAPI, AMD)
# ==========================================
FROM linuxserver/ffmpeg:version-7.1-cli

LABEL maintainer="MediaCruncher Team"
LABEL description="Automated distributed media transcoding engine with full GPU acceleration"

# Environment variables for NVIDIA Container Toolkit
ENV NVIDIA_VISIBLE_DEVICES=all
ENV NVIDIA_DRIVER_CAPABILITIES=compute,video,utility

# Create dedicated application directories
RUN mkdir -p /etc/mediacruncher \
             /var/lib/mediacruncher \
             /tmp/mediacruncher/staging \
             /media

# Install binary from builder stage
COPY --from=builder /mediacruncher /usr/local/bin/mediacruncher

# Expose metrics & healthz HTTP port
EXPOSE 9090

# Pre-declare mount volumes
VOLUME ["/etc/mediacruncher", "/var/lib/mediacruncher", "/tmp/mediacruncher/staging", "/media"]

ENTRYPOINT ["/usr/local/bin/mediacruncher"]
CMD ["daemon", "-config", "/etc/mediacruncher/config.yaml"]

