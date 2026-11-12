# sim-ca: issues the run's throwaway CA, the service certificates and their
# pins into the tls volume, once. sim-aimem's certificate also names
# sim-px-aimem and sim-px-hub, the fault proxies the members' aimem and
# aicrewd reach it through, so the hub's name still checks.
FROM debian:bookworm-20261020-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
COPY simca /usr/local/bin/simca
ENTRYPOINT ["simca", "-out", "/sim/tls", "-leaf", "sim-aimem,sim-px-aimem,sim-px-hub", "-leaf", "sim-aicrewd", "-leaf", "sim-gitea"]
