# syntax=docker/dockerfile:1.18
ARG GO_IMAGE=docker.io/library/golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452
ARG NODE_IMAGE=docker.io/library/node:24.8.0-bookworm-slim@sha256:cadbfafeb6baf87eaaffa40b3640209c4b7fd38cebde65059d15bc39cd636b85
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
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/devsandbox-agent ./cmd/devsandbox-agent && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/devsandbox-copilot ./cmd/devsandbox-copilot && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/devsandbox-init ./cmd/devsandbox-init

FROM docker.io/library/node:24.8.0-bookworm-slim@sha256:cadbfafeb6baf87eaaffa40b3640209c4b7fd38cebde65059d15bc39cd636b85 AS node-runtime

FROM docker.io/library/ubuntu:24.04@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517
ARG TARGETARCH
ARG GH_VERSION=2.99.0
ARG GH_SHA256_AMD64=ed4960225d2833e04a61590d9fa2b5773d147f3aa375459e5466a40c102f3832
ARG GH_SHA256_ARM64=564eff56a61e8caf193efde16937fba879eb62a3a479c9dd6be2001e7647680b
ARG COPILOT_VERSION=1.0.82
ARG COPILOT_SHA256_AMD64=37fa67686a9e4ed8d46dcd6a9c80ab524dea840ecaa0a3f7edf8d09f961b97a9
ARG COPILOT_SHA256_ARM64=86c4c77a91b1ff5dd7313cbea9f861699a2cdceb4696f3864f3f0584e9684e0f
ARG PLAYWRIGHT_CLI_VERSION=0.1.19
ARG PLAYWRIGHT_CLI_SHA256=0a6fca06371fa7e69be33f6730f78d7bdd69d037390045c4050e961e8b1dfeee
ARG PLAYWRIGHT_VERSION=1.63.0-alpha-2026-08-31
ARG NPM_REGISTRY=https://packagefeedproxy.microsoft.io/npm/
ARG BUILD_DATE=unknown
ARG REVISION=unknown
ARG TEMPLATE_VERSION=0.1.0
LABEL org.opencontainers.image.title="DevSandbox Copilot template" \
      org.opencontainers.image.description="Curated DevSandbox environment with GitHub Copilot CLI and Playwright Chromium" \
      org.opencontainers.image.source="https://github.com/rmoreirao/AKSAgentSandbox" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${TEMPLATE_VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      devsandbox.io/github-cli.version="${GH_VERSION}" \
      devsandbox.io/copilot-cli.version="${COPILOT_VERSION}" \
      devsandbox.io/playwright-cli.version="${PLAYWRIGHT_CLI_VERSION}" \
      devsandbox.io/playwright.version="${PLAYWRIGHT_VERSION}"

COPY images/common/install-base.sh /usr/local/libexec/install-devsandbox-base
RUN chmod 0755 /usr/local/libexec/install-devsandbox-base && \
    /usr/local/libexec/install-devsandbox-base && rm /usr/local/libexec/install-devsandbox-base
COPY --from=node-runtime /usr/local/ /usr/local/
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) gh_checksum="${GH_SHA256_AMD64}"; copilot_arch=x64; copilot_checksum="${COPILOT_SHA256_AMD64}" ;; \
      arm64) gh_checksum="${GH_SHA256_ARM64}"; copilot_arch=arm64; copilot_checksum="${COPILOT_SHA256_ARM64}" ;; \
      *) echo "Unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
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
    npm config set registry "${NPM_REGISTRY}"; \
    npm pack "@playwright/cli@${PLAYWRIGHT_CLI_VERSION}" --pack-destination /tmp; \
    playwright_archive="/tmp/playwright-cli-${PLAYWRIGHT_CLI_VERSION}.tgz"; \
    echo "${PLAYWRIGHT_CLI_SHA256}  ${playwright_archive}" | sha256sum --check --strict; \
    npm install --global --omit=dev --ignore-scripts "${playwright_archive}"; \
    rm "${playwright_archive}"; \
    test "$(node -p "require('/usr/local/lib/node_modules/@playwright/cli/node_modules/playwright/package.json').version")" = "${PLAYWRIGHT_VERSION}"; \
    PLAYWRIGHT_BROWSERS_PATH=/opt/playwright \
      /usr/local/lib/node_modules/@playwright/cli/node_modules/playwright/cli.js install --with-deps chromium; \
    npm cache clean --force; \
    rm -rf /var/lib/apt/lists/* /root/.cache

COPY images/common/git-credential-devsandbox /usr/local/bin/git-credential-devsandbox
COPY images/common/gh-wrapper /usr/local/bin/gh
COPY images/common/copilot-wrapper /usr/local/bin/copilot
RUN chmod 0755 /usr/local/bin/git-credential-devsandbox /usr/local/bin/gh /usr/local/bin/copilot
COPY --from=platform-binaries /out/devsandbox-copilot /usr/local/libexec/devsandbox-copilot
COPY --from=platform-binaries /out/devsandbox-agent /out/devsandbox-init /usr/local/bin/
RUN chmod 0755 /usr/local/libexec/devsandbox-copilot /usr/local/bin/devsandbox-agent /usr/local/bin/devsandbox-init

ENV HOME=/home/devsandbox \
    SHELL=/bin/bash \
    DEVSANDBOX_RUNTIME_DIR=/run/devsandbox \
    DEVSANDBOX_GITHUB_TOKEN_FILE=/run/devsandbox/github-token \
    COPILOT_HOME=/run/devsandbox/copilot \
    PLAYWRIGHT_BROWSERS_PATH=/opt/playwright
WORKDIR /workspace
USER 1000:1000
CMD ["/bin/bash"]
