#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
  bash \
  ca-certificates \
  coreutils \
  curl \
  file \
  findutils \
  gh \
  git \
  git-lfs \
  gzip \
  jq \
  less \
  netcat-openbsd \
  openssh-client \
  procps \
  sed \
  tar \
  tini \
  unzip \
  xz-utils
rm -rf /var/lib/apt/lists/*

install -m 0755 "$(command -v gh)" /usr/local/libexec/gh
rm -f "$(command -v gh)"

existing_user="$(getent passwd 1000 | cut -d: -f1 || true)"
if [[ -n "${existing_user}" ]]; then
  userdel --remove "${existing_user}" 2>/dev/null || userdel "${existing_user}"
fi
existing_group="$(getent group 1000 | cut -d: -f1 || true)"
if [[ -n "${existing_group}" ]]; then
  groupdel "${existing_group}"
fi
groupadd --gid 1000 devsandbox
useradd --uid 1000 --gid 1000 --create-home --shell /bin/bash devsandbox

install -d -o 1000 -g 1000 /workspace /home/devsandbox/.config
git lfs install --system --skip-repo
git config --system credential.helper devsandbox
git config --system credential.useHttpPath true
