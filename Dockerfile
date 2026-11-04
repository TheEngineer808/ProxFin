# First stage. Building a binary
# -----------------------------------------------------------------------------
ARG USE_NVIDIA

FROM golang:1.23-bullseye AS builder

WORKDIR /app

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY . .

# Build the application with optimizations
RUN CGO_ENABLED=0 go build -mod=mod -ldflags="-s -w" -trimpath -o proxfin .

# Second stage. Creating a minimal image
# -----------------------------------------------------------------------------
ARG USE_NVIDIA
FROM ubuntu:24.04 AS standard
FROM nvidia/cuda:12.8.0-base-ubuntu24.04 AS nvidia
FROM standard AS final
FROM nvidia AS final-nvidia

ARG USE_NVIDIA
FROM final${USE_NVIDIA:+-nvidia}

ARG BUILD_DATE
ARG VCS_REF
ARG PROXFIN_PORT=34400
ARG PROXFIN_VERSION

LABEL org.label-schema.build-date="${BUILD_DATE}" \
      org.label-schema.name="ProxFin" \
      org.label-schema.description="M3U proxy for Plex DVR, Emby and Jellyfin Live TV (based on Threadfin and xTeVe)" \
      org.label-schema.url="https://github.com/TheEngineer808/ProxFin" \
      org.label-schema.vcs-ref="${VCS_REF}" \
      org.label-schema.vcs-url="https://github.com/TheEngineer808/ProxFin" \
      org.label-schema.vendor="ProxFin" \
      org.label-schema.version="${PROXFIN_VERSION}" \
      org.label-schema.schema-version="1.0"

ENV PROXFIN_BIN=/home/proxfin/bin \
    PROXFIN_CONF=/home/proxfin/conf \
    PROXFIN_HOME=/home/proxfin \
    PROXFIN_TEMP=/tmp/proxfin \
    PROXFIN_CACHE=/home/proxfin/cache \
    PROXFIN_DEBUG=0 \
    PROXFIN_PORT=34400 \
    PROXFIN_BIND_IP_ADDRESS=0.0.0.0 \
    NVIDIA_DRIVER_CAPABILITIES=compute,video,utility \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/home/proxfin/bin \
    DEBIAN_FRONTEND=noninteractive

# Set working directory
WORKDIR $PROXFIN_HOME

# Arguments to add the jellyfin repository
ARG TARGETARCH
ARG OS_VERSION=ubuntu
ARG OS_CODENAME=noble

# Install dependencies in a single layer
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    ffmpeg \
    vlc \
    tzdata \
    gnupg \
    apt-transport-https && \
    mkdir -p $PROXFIN_BIN $PROXFIN_CONF $PROXFIN_TEMP $PROXFIN_HOME/cache && \
    chmod a+rwX $PROXFIN_CONF $PROXFIN_TEMP && \
    sed -i 's/geteuid/getppid/' /usr/bin/vlc && \
    curl -fsSL https://repo.jellyfin.org/master/ubuntu/jellyfin_team.gpg.key \
        | gpg --dearmor -o /etc/apt/trusted.gpg.d/ubuntu-jellyfin.gpg && \
    (if [ "${TARGETARCH}" = "arm" ]; then \
        echo "deb [arch=armhf] https://repo.jellyfin.org/master/${OS_VERSION} ${OS_CODENAME} main" > /etc/apt/sources.list.d/jellyfin.list; \
    else \
        echo "deb [arch=${TARGETARCH}] https://repo.jellyfin.org/master/${OS_VERSION} ${OS_CODENAME} main" > /etc/apt/sources.list.d/jellyfin.list; \
    fi) && \
    apt-get update && \
    apt-get install --no-install-recommends --no-install-suggests --yes \
        jellyfin-ffmpeg7 && \
    apt-get remove gnupg apt-transport-https --yes  && \
    apt-get clean autoclean --yes && \
    apt-get autoremove --yes && \
    rm -rf /var/cache/apt/archives* /var/lib/apt/lists/*

# Copy built binary from builder image
COPY --from=builder /app/proxfin $PROXFIN_BIN/
COPY docker-entrypoint.sh /usr/local/bin/proxfin-entrypoint
RUN chmod +rx $PROXFIN_BIN/proxfin /usr/local/bin/proxfin-entrypoint

# Configure container volume mappings
VOLUME $PROXFIN_CONF
VOLUME $PROXFIN_TEMP

EXPOSE $PROXFIN_PORT

# Uses /home/threadfin/conf instead when a Threadfin volume is mounted there
ENTRYPOINT ["/usr/local/bin/proxfin-entrypoint"]
