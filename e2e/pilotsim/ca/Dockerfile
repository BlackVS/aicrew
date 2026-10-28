# sim-ca: issues the run's throwaway CA, the service certificates and their
# pins into the tls volume, once.
FROM debian:bookworm-20261020-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
COPY simca /usr/local/bin/simca
ENTRYPOINT ["simca", "-out", "/sim/tls", "-leaf", "sim-aimem", "-leaf", "sim-aicrewd", "-leaf", "sim-gitea"]
