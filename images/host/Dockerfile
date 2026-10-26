FROM scratch AS runtime-build
COPY --from=runtime /morde-runtime /usr/local/bin/morde-runtime

FROM ghcr.io/agent-infra/sandbox:1.11.0@sha256:6328d7fd2f0ff0b4c147c3d05b3df1ce331f4a482eb6e550ecd64ed1fcf906e7 AS bubblewrap-build
RUN apt-get update \
    && printf '%s\n' 'deb-src http://ports.ubuntu.com/ubuntu-ports noble main universe' > /etc/apt/sources.list.d/noble-src.list \
    && apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libcap-dev libselinux1-dev pkg-config \
    && cd /tmp \
    && apt-get source bubblewrap=0.9.0-1build1 \
    && meson setup /tmp/bubblewrap-0.9.0/build /tmp/bubblewrap-0.9.0 -Dman=disabled -Dtests=false \
    && meson compile -C /tmp/bubblewrap-0.9.0/build

FROM ghcr.io/agent-infra/sandbox:1.11.0@sha256:6328d7fd2f0ff0b4c147c3d05b3df1ce331f4a482eb6e550ecd64ed1fcf906e7

ENV TZ=Asia/Shanghai \
    WORKSPACE=/home/gem/workspace

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        bubblewrap \
        fonts-noto-cjk \
        git \
        jq \
        ripgrep \
        tzdata \
        util-linux \
    && ln -snf "/usr/share/zoneinfo/${TZ}" /etc/localtime \
    && printf '%s\n' "${TZ}" > /etc/timezone \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

ARG CODEX_CLI_VERSION=0.164.0
ARG TARGETARCH
RUN set -eu; \
    case "$TARGETARCH" in \
        amd64) codex_asset_arch=x86_64; codex_sha256=90dd0b03b28afcb7fa733a4f73b413db2916a47347efd43fb669cc1c774ee8e4 ;; \
        arm64) codex_asset_arch=aarch64; codex_sha256=29a507a2a3fd8f6cd94b96b762d281d299d15d7902dee6132d314b78cf9108eb ;; \
        *) echo "unsupported Codex CLI architecture: $TARGETARCH" >&2; exit 2 ;; \
    esac; \
    archive="/tmp/codex-${TARGETARCH}.tar.gz"; \
    curl -fsSL "https://github.com/openai/codex/releases/download/rust-v${CODEX_CLI_VERSION}/codex-${codex_asset_arch}-unknown-linux-musl.tar.gz" -o "$archive"; \
    printf '%s  %s\n' "$codex_sha256" "$archive" | sha256sum -c -; \
    tar -xzf "$archive" -C /tmp; \
    install -m 0755 "/tmp/codex-${codex_asset_arch}-unknown-linux-musl" /opt/nodejs/22/bin/codex; \
    rm -f "$archive" "/tmp/codex-${codex_asset_arch}-unknown-linux-musl"; \
    version_output="$(codex --version 2>&1)"; \
    printf '%s\n' "$version_output"; \
    case "$version_output" in *"$CODEX_CLI_VERSION"*) ;; *) exit 1 ;; esac

WORKDIR /home/gem/workspace
EXPOSE 8090

COPY --chmod=755 healthcheck.sh /usr/local/bin/cloudhost-healthcheck
# Jammy's packaged 0.6.1 lacks --disable-userns, so Runtime uses the pinned 0.9.0 binary.
COPY --from=bubblewrap-build --chmod=755 /tmp/bubblewrap-0.9.0/build/bwrap /usr/bin/bwrap
COPY --from=runtime-build --chmod=755 /usr/local/bin/morde-runtime /usr/local/bin/morde-runtime
COPY --chmod=755 morde-host-entrypoint.sh /usr/local/bin/morde-host-entrypoint
COPY --chmod=644 morde-runtime-bootstrap.sh /usr/local/lib/morde-runtime-bootstrap.sh
COPY --chmod=755 morde-aio-profile-config.py /usr/local/bin/morde-aio-profile-config
COPY --chmod=755 morde-aio-profile-hook.sh /usr/local/bin/morde-aio-profile-hook

ENTRYPOINT ["/usr/local/bin/morde-host-entrypoint"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=90s --retries=3 \
    CMD ["/usr/local/bin/cloudhost-healthcheck"]
