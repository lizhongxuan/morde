FROM scratch

LABEL org.opencontainers.image.licenses="MPL-2.0"

COPY syncthing /usr/local/bin/syncthing
COPY LICENSE /usr/share/licenses/MPL-2.0

ENV HOME=/var/syncthing \
    STHOMEDIR=/var/syncthing/config \
    STNOUPGRADE=1

USER 1000:1000
ENTRYPOINT ["/usr/local/bin/syncthing"]
CMD ["serve", "--no-browser", "--no-restart"]
