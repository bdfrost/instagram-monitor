# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Cache dependencies layer
COPY go.mod go.sum* ./
RUN go mod download 2>/dev/null || true

COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/monitor ./cmd/monitor

# Runtime stage - minimal image
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata

RUN addgroup -g 1000 appgroup && \
    adduser -u 1000 -G appgroup -s /bin/sh -D appuser

WORKDIR /app

COPY --from=builder /app/monitor /usr/local/bin/instagram-monitor

# Create directories for config and state
RUN mkdir -p /app/config /app/state

ARG BUILD_VERSION=dev
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="instagram-monitor"
LABEL org.opencontainers.image.description="Instagram feed monitor for appointment booking alerts"
LABEL org.opencontainers.image.source="https://github.com/bdfrost/instagram-monitor"
LABEL org.opencontainers.image.version="${BUILD_VERSION}"
LABEL org.opencontainers.image.created="${BUILD_DATE}"

ENTRYPOINT ["/usr/local/bin/instagram-monitor"]
CMD ["--config", "/app/config/config.json"]
