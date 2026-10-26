FROM ghcr.io/agent-infra/sandbox:1.11.0@sha256:6328d7fd2f0ff0b4c147c3d05b3df1ce331f4a482eb6e550ecd64ed1fcf906e7

COPY packages/WeChatLinux_arm64.deb /tmp/WeChatLinux_arm64.deb
COPY packages/CloudHub_arm64_5.0.5_2601301023.deb /tmp/CloudHub_arm64_5.0.5_2601301023.deb

RUN set -eu; \
    test "$(dpkg --print-architecture)" = arm64; \
    printf '%s  %s\n' \
      374732eda88a6f3a7e4eb315e64e5fac73ef17923d504d242964f19ed1b80c75 /tmp/WeChatLinux_arm64.deb \
      105d0ed2263f5a56faa742347f6739cfa98b623b4a97b4b24868e4e50e75e7c5 /tmp/CloudHub_arm64_5.0.5_2601301023.deb | sha256sum -c -; \
    sed -i 's#http://ports.ubuntu.com/ubuntu-ports/#https://ports.ubuntu.com/ubuntu-ports/#g' /etc/apt/sources.list; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      /tmp/WeChatLinux_arm64.deb \
      /tmp/CloudHub_arm64_5.0.5_2601301023.deb \
      at-spi2-core \
      python3-pyatspi; \
    apt-get clean; \
    rm -f /tmp/WeChatLinux_arm64.deb /tmp/CloudHub_arm64_5.0.5_2601301023.deb; \
    rm -rf /var/lib/apt/lists/*
