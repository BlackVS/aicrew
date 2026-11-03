# sim-px-aimem, sim-px-crew and sim-px-peer: the fault proxies on the TLS
# boundaries of DESIGN-PILOT-SIMULATION section 6.1, one image; each
# service's command names its listener, certificate and target. It runs as
# root to read the target's owner-only key from the tls volume.
FROM debian:bookworm-20261020-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
RUN install -d -m 700 /var/lib/simproxy
COPY simproxy /usr/local/bin/simproxy
ENTRYPOINT ["simproxy", "serve", "-ca", "/sim/tls/ca.pem"]
