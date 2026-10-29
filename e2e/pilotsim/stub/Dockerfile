# sim-hands and sim-ci: the stub reviewer and the stub CI
# (DESIGN-PILOT-SIMULATION section 2.5), one image, the mode set by
# SIM_STUB. Gitea's certificate is trusted through SSL_CERT_FILE.
FROM debian:bookworm-20261020-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
RUN useradd -m -u 3100 -s /bin/sh stub \
    && install -d -o stub -g stub -m 700 /sim/in /sim/scenario
COPY simstub /usr/local/bin/simstub
COPY stub.sh /usr/local/bin/stub.sh
USER stub
ENTRYPOINT ["/bin/sh", "/usr/local/bin/stub.sh"]
