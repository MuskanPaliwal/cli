FROM mcr.microsoft.com/devcontainers/base:ubuntu-24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    build-essential \
    ca-certificates \
    curl \
    dbus-x11 \
    git \
    gnome-keyring \
    jq \
    less \
    openssh-client \
    pkg-config \
    shellcheck \
    sudo \
    tmux \
    unzip \
    xz-utils \
    zip \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh

ENV MISE_DATA_DIR=/home/vscode/.local/share/mise
ENV PATH=/home/vscode/.local/share/mise/shims:/home/vscode/.local/bin:/home/vscode/go/bin:${PATH}

USER vscode
