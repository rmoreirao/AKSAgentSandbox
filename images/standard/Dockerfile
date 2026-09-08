# syntax=docker/dockerfile:1.18
ARG GO_IMAGE=docker.io/library/golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452
ARG RUNTIME_IMAGE=docker.io/library/ubuntu:24.04@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517

FROM docker.io/library/golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452 AS platform-binaries
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ ./api/
COPY cmd/devsandbox-agent/ ./cmd/devsandbox-agent/
COPY cmd/devsandbox-init/ ./cmd/devsandbox-init/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/devsandbox-agent ./cmd/devsandbox-agent && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/devsandbox-init ./cmd/devsandbox-init

FROM docker.io/library/ubuntu:24.04@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517
ARG BUILD_DATE=unknown
ARG REVISION=unknown
ARG TEMPLATE_VERSION=0.1.0
LABEL org.opencontainers.image.title="DevSandbox standard template" \
      org.opencontainers.image.description="Curated non-root DevSandbox command-line environment" \
      org.opencontainers.image.source="https://github.com/rmoreirao/AKSAgentSandbox" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${TEMPLATE_VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY images/common/install-base.sh /usr/local/libexec/install-devsandbox-base
RUN chmod 0755 /usr/local/libexec/install-devsandbox-base && \
    /usr/local/libexec/install-devsandbox-base && rm /usr/local/libexec/install-devsandbox-base
COPY images/common/git-credential-devsandbox /usr/local/bin/git-credential-devsandbox
COPY images/common/gh-wrapper /usr/local/bin/gh
RUN chmod 0755 /usr/local/bin/git-credential-devsandbox /usr/local/bin/gh
COPY --from=platform-binaries /out/devsandbox-agent /out/devsandbox-init /usr/local/bin/
RUN chmod 0755 /usr/local/bin/devsandbox-agent /usr/local/bin/devsandbox-init

ENV HOME=/home/devsandbox \
    DEVSANDBOX_RUNTIME_DIR=/run/devsandbox \
    DEVSANDBOX_GITHUB_TOKEN_FILE=/run/devsandbox/github-token
WORKDIR /workspace
USER 1000:1000
CMD ["/bin/bash"]
