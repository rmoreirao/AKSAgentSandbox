ARG BUILDER_IMAGE=docker.io/library/golang:1.26.2@sha256:b54cbf583d390341599d7bcbc062425c081105cc5ef6d170ced98ef9d047c716
ARG RUNTIME_IMAGE=gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7

FROM docker.io/library/golang:1.26.2@sha256:b54cbf583d390341599d7bcbc062425c081105cc5ef6d170ced98ef9d047c716 AS builder
ARG TARGETARCH
ARG GIT_VERSION=v1.0.0
ARG GIT_SHA=bb72f49d79f009a960eed2ae6c32e1cc082399c5
ARG BUILD_DATE=unknown
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY sandbox-router/ ./sandbox-router/
COPY api/ ./api/
COPY extensions/api/ ./extensions/api/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w -X sigs.k8s.io/agent-sandbox/internal/version.gitVersion=${GIT_VERSION} -X sigs.k8s.io/agent-sandbox/internal/version.gitSHA=${GIT_SHA} -X sigs.k8s.io/agent-sandbox/internal/version.buildDate=${BUILD_DATE}" \
    -o /sandbox-router ./sandbox-router/cmd

FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
ARG GIT_SHA=bb72f49d79f009a960eed2ae6c32e1cc082399c5
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.source="https://github.com/kubernetes-sigs/agent-sandbox" \
      org.opencontainers.image.title="Sandbox Router" \
      org.opencontainers.image.revision="${GIT_SHA}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.version="v1.0.0"
COPY --from=builder /sandbox-router /sandbox-router
EXPOSE 8080 9090 8081
ENTRYPOINT ["/sandbox-router"]
