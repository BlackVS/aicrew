# sim-aimem: the hub, installed by the pinned aimem release's install-hub.sh
# in its install-only mode (AIMEM_NO_SERVICE=1), from the staged release
# asset (DESIGN-PILOT-SIMULATION section 3); hub.sh runs it on the first
# start and execs the serve command it prints.
FROM debian:bookworm-20261020-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 2001 -s /bin/bash aimem
COPY aimem-release/ /opt/aimem-release/
COPY install-hub.sh /opt/aimem-install-hub.sh
COPY process/ /opt/process/
COPY hub.sh /usr/local/sbin/hub.sh
EXPOSE 8440
ENTRYPOINT ["bash", "/usr/local/sbin/hub.sh"]
