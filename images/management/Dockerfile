# syntax=docker/dockerfile:1.18
ARG GO_IMAGE=docker.io/library/golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM docker.io/library/golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452 AS builder
ARG TARGETOS=linux
ARG TARGETARCH
ARG BINARY
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ ./api/
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN case "${BINARY}" in \
      devsandbox-api|devsandbox-broker|devsandbox-operator|devsandbox-web) ;; \
      *) echo "Unsupported management binary: ${BINARY}" >&2; exit 1 ;; \
    esac && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/app "./cmd/${BINARY}"

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG BINARY
ARG BUILD_DATE=unknown
ARG REVISION=unknown
ARG IMAGE_VERSION=0.1.0
LABEL org.opencontainers.image.title="${BINARY}" \
      org.opencontainers.image.source="https://github.com/rmoreirao/AKSAgentSandbox" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${IMAGE_VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}"
COPY --from=builder /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
