# sim-aimem: the hub, from the pinned aimem release (DESIGN-PILOT-SIMULATION
# section 3). install-hub.sh needs systemd, which a plain container lacks
# (aimem task 01a18163-87ac), so hub.sh does what it does without the
# service: the asset checked against SHA256SUMS, the env file, aimem serve.
FROM debian:bookworm-20261020-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 2001 -s /bin/bash aimem
COPY aimem-release/ /opt/aimem-release/
COPY process/ /opt/process/
COPY hub.sh /usr/local/sbin/hub.sh
EXPOSE 8440
ENTRYPOINT ["bash", "/usr/local/sbin/hub.sh"]
