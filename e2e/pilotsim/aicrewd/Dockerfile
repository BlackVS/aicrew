# sim-aicrewd: aicrewd installed by install-aicrewd.sh in its install-only
# mode from this tree's binaries (DESIGN-PILOT-SIMULATION section 3); the
# container is its supervisor.
FROM debian:bookworm-20261020-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 2002 -s /bin/bash sessiond
COPY prebuilt/ /opt/aicrew-prebuilt/
COPY install-aicrewd.sh /opt/install-aicrewd.sh
COPY aicrewd.sh /usr/local/sbin/aicrewd.sh
EXPOSE 9443
ENTRYPOINT ["bash", "/usr/local/sbin/aicrewd.sh"]
