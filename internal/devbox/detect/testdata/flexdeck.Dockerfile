# ==============================================================================
# FRONTEND BUILD STAGE
# ==============================================================================
FROM --platform=$BUILDPLATFORM node:20-alpine AS frontend-builder

WORKDIR /app/web

# Install dependencies with cache mount
COPY web/package*.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --prefer-offline

# Build frontend (source changes trigger rebuild, but deps are cached)
COPY web/ ./
RUN npm run build

# ==============================================================================
# BACKEND BUILD STAGE
# ==============================================================================
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS backend-builder

WORKDIR /app

# Use the in-cluster Go module proxy (Athens) first, but allow resilient fallback.
# `|` tells Go to fallback on any error (including DNS/network), not only 404/410.
# Allow override for local builds outside the cluster.
ARG GOPROXY="http://athens.ci.svc.cluster.local:3000|https://proxy.golang.org,direct"
ENV GOPROXY=${GOPROXY}

# Install build dependencies
RUN apk add --no-cache git ca-certificates

# Download Go modules (cached separately from source)
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Build binary with caches
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -trimpath -o /flexdeck ./cmd/server

# ==============================================================================
# RUNTIME STAGE (minimal)
# ==============================================================================
FROM alpine:3.21 AS runtime

# Install runtime dependencies in single layer
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 1000 flexdeck

WORKDIR /app

# Copy artifacts from builders
COPY --from=backend-builder --chown=flexdeck:flexdeck /flexdeck /app/flexdeck
COPY --from=frontend-builder --chown=flexdeck:flexdeck /app/web/dist /app/web/dist

USER flexdeck

ENV PORT=8080 \
    STATIC_DIR=/app/web/dist

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -q --spider http://localhost:8080/api/health || exit 1

ENTRYPOINT ["/app/flexdeck"]
