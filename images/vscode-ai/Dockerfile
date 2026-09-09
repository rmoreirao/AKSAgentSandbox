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
COPY cmd/devsandbox-copilot/ ./cmd/devsandbox-copilot/
COPY cmd/devsandbox-init/ ./cmd/devsandbox-init/
COPY cmd/devsandbox-opencode/ ./cmd/devsandbox-opencode/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/devsandbox-agent ./cmd/devsandbox-agent && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/devsandbox-copilot ./cmd/devsandbox-copilot && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/devsandbox-init ./cmd/devsandbox-init && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/devsandbox-opencode ./cmd/devsandbox-opencode

FROM docker.io/library/ubuntu:24.04@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517
ARG TARGETARCH
ARG CODE_SERVER_VERSION=4.135.0
ARG CODE_SERVER_SHA256_AMD64=300ef4e37e469e6368a4673c6a623e1c9ba8a34f42b394fb49c431a8900bc7d1
ARG CODE_SERVER_SHA256_ARM64=fe6561798415e709109cb902dca2a57a687240af7d8220f6fa1d01cd2ae0541e
ARG GH_VERSION=2.99.0
ARG GH_SHA256_AMD64=ed4960225d2833e04a61590d9fa2b5773d147f3aa375459e5466a40c102f3832
ARG GH_SHA256_ARM64=564eff56a61e8caf193efde16937fba879eb62a3a479c9dd6be2001e7647680b
ARG COPILOT_VERSION=1.0.82
ARG COPILOT_SHA256_AMD64=37fa67686a9e4ed8d46dcd6a9c80ab524dea840ecaa0a3f7edf8d09f961b97a9
ARG COPILOT_SHA256_ARM64=86c4c77a91b1ff5dd7313cbea9f861699a2cdceb4696f3864f3f0584e9684e0f
ARG OPENCODE_VERSION=1.18.30
ARG OPENCODE_SHA256_AMD64=55007246858165496ff85ba1c2b648f7421e8e2013bf4189a680c9ff8e699d17
ARG OPENCODE_SHA256_ARM64=4111a55c2a02c0fac314bd51e9a2330280e6d29d2b85b9554fff6d62612566ed
ARG BUILD_DATE=unknown
ARG REVISION=unknown
ARG TEMPLATE_VERSION=0.1.0
LABEL org.opencontainers.image.title="DevSandbox VS Code AI template" \
      org.opencontainers.image.description="Curated browser VS Code environment with GitHub Copilot CLI and OpenCode" \
      org.opencontainers.image.source="https://github.com/rmoreirao/AKSAgentSandbox" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${TEMPLATE_VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      devsandbox.io/code-server.version="${CODE_SERVER_VERSION}" \
      devsandbox.io/github-cli.version="${GH_VERSION}" \
      devsandbox.io/copilot-cli.version="${COPILOT_VERSION}" \
      devsandbox.io/opencode.version="${OPENCODE_VERSION}"

COPY images/common/install-base.sh /usr/local/libexec/install-devsandbox-base
RUN chmod 0755 /usr/local/libexec/install-devsandbox-base && \
    /usr/local/libexec/install-devsandbox-base && rm /usr/local/libexec/install-devsandbox-base
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) \
        code_server_checksum="${CODE_SERVER_SHA256_AMD64}"; \
        gh_checksum="${GH_SHA256_AMD64}"; \
        copilot_arch=x64; \
        copilot_checksum="${COPILOT_SHA256_AMD64}"; \
        opencode_arch=x64; \
        opencode_checksum="${OPENCODE_SHA256_AMD64}" ;; \
      arm64) \
        code_server_checksum="${CODE_SERVER_SHA256_ARM64}"; \
        gh_checksum="${GH_SHA256_ARM64}"; \
        copilot_arch=arm64; \
        copilot_checksum="${COPILOT_SHA256_ARM64}"; \
        opencode_arch=arm64; \
        opencode_checksum="${OPENCODE_SHA256_ARM64}" ;; \
      *) echo "Unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    code_server_archive="code-server-${CODE_SERVER_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    curl --fail --show-error --silent --location \
      "https://github.com/coder/code-server/releases/download/v${CODE_SERVER_VERSION}/${code_server_archive}" \
      --output "/tmp/${code_server_archive}"; \
    echo "${code_server_checksum}  /tmp/${code_server_archive}" | sha256sum --check --strict; \
    tar -xzf "/tmp/${code_server_archive}" -C /opt; \
    mv "/opt/code-server-${CODE_SERVER_VERSION}-linux-${TARGETARCH}" /opt/code-server; \
    ln -s /opt/code-server/bin/code-server /usr/local/bin/code-server; \
    rm "/tmp/${code_server_archive}"; \
    gh_archive="gh_${GH_VERSION}_linux_${TARGETARCH}.tar.gz"; \
    curl --fail --show-error --silent --location \
      "https://github.com/cli/cli/releases/download/v${GH_VERSION}/${gh_archive}" \
      --output "/tmp/${gh_archive}"; \
    echo "${gh_checksum}  /tmp/${gh_archive}" | sha256sum --check --strict; \
    tar -xzf "/tmp/${gh_archive}" -C /tmp; \
    install -m 0755 "/tmp/gh_${GH_VERSION}_linux_${TARGETARCH}/bin/gh" /usr/local/libexec/gh; \
    rm -rf "/tmp/${gh_archive}" "/tmp/gh_${GH_VERSION}_linux_${TARGETARCH}"; \
    copilot_archive="copilot-linux-${copilot_arch}.tar.gz"; \
    curl --fail --show-error --silent --location \
      "https://github.com/github/copilot-cli/releases/download/v${COPILOT_VERSION}/${copilot_archive}" \
      --output "/tmp/${copilot_archive}"; \
    echo "${copilot_checksum}  /tmp/${copilot_archive}" | sha256sum --check --strict; \
    tar -xzf "/tmp/${copilot_archive}" -C /usr/local/libexec; \
    test -x /usr/local/libexec/copilot; \
    rm "/tmp/${copilot_archive}"; \
    opencode_archive="opencode-linux-${opencode_arch}.tar.gz"; \
    curl --fail --show-error --silent --location \
      "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/${opencode_archive}" \
      --output "/tmp/${opencode_archive}"; \
    echo "${opencode_checksum}  /tmp/${opencode_archive}" | sha256sum --check --strict; \
    tar -xzf "/tmp/${opencode_archive}" -C /usr/local/libexec; \
    test -x /usr/local/libexec/opencode; \
    rm "/tmp/${opencode_archive}"; \
    rm -rf /var/lib/apt/lists/*

COPY images/common/git-credential-devsandbox /usr/local/bin/git-credential-devsandbox
COPY images/common/gh-wrapper /usr/local/bin/gh
COPY images/common/copilot-wrapper /usr/local/bin/copilot
COPY images/common/opencode-wrapper /usr/local/bin/opencode
COPY images/common/code-server-entrypoint /usr/local/bin/devsandbox-code-server
RUN chmod 0755 /usr/local/bin/git-credential-devsandbox /usr/local/bin/gh \
    /usr/local/bin/copilot /usr/local/bin/opencode /usr/local/bin/devsandbox-code-server
COPY --from=platform-binaries /out/devsandbox-copilot /out/devsandbox-opencode /usr/local/libexec/
COPY --from=platform-binaries /out/devsandbox-agent /out/devsandbox-init /usr/local/bin/
RUN chmod 0755 /usr/local/libexec/devsandbox-copilot /usr/local/libexec/devsandbox-opencode \
    /usr/local/bin/devsandbox-agent /usr/local/bin/devsandbox-init

ENV HOME=/home/devsandbox \
    SHELL=/bin/bash \
    DEVSANDBOX_RUNTIME_DIR=/run/devsandbox \
    DEVSANDBOX_GITHUB_TOKEN_FILE=/run/devsandbox/github-token \
    DEVSANDBOX_OPENCODE_RUNTIME_DIR=/run/devsandbox-opencode \
    COPILOT_HOME=/run/devsandbox/copilot \
    DEVSANDBOX_VSCODE_STATE_DIR=/workspace/.devsandbox/vscode
EXPOSE 13337
WORKDIR /workspace
USER 1000:1000
CMD ["/bin/bash"]
