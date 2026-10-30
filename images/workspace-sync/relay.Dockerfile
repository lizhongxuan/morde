FROM scratch

LABEL org.opencontainers.image.licenses="MPL-2.0"

COPY strelaysrv /usr/local/bin/strelaysrv
COPY LICENSE /usr/share/licenses/MPL-2.0

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/strelaysrv"]
