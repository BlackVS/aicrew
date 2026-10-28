# A member home (sim-coordinator, sim-worker): git and Python 3, the
# client stand-in as `claude` on PATH, then each component by its own
# installer, as aicrew-agent check instructs (DESIGN-PILOT-SIMULATION
# section 3): aicrew's boot.sh from this tree's binaries, aimem's
# verifying boot script at the tested release, ai-skills from its release
# archive checked against SHA256SUMS. The archive's install.sh has no
# execute bit, so it runs with bash, not as check's `./src/install.sh`
# instruction says (aicrew task 01a18173-b01e). aimem's installer needs jq.
FROM debian:bookworm-20261020-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
ARG AIMEM_VERSION
ARG AISKILLS_VERSION
ARG AISKILLS
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl git jq python3 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 3000 -s /bin/bash member \
    && install -d -o member -g member -m 700 /sim/in
COPY claude /usr/local/bin/claude
COPY prebuilt/ /opt/aicrew-prebuilt/
COPY boot.sh /opt/aicrew-boot.sh
COPY member.sh /usr/local/bin/member.sh
USER member
WORKDIR /home/member
ENV PATH=/home/member/.local/bin:/usr/local/bin:/usr/bin:/bin
RUN AICREW_PREBUILT_DIR=/opt/aicrew-prebuilt AICREW_NO_SERVICE=1 bash /opt/aicrew-boot.sh
RUN cd "$(mktemp -d)" && curl -fsSL https://raw.githubusercontent.com/BlackVS/aimem/master/boot.sh \
    | AIMEM_VERSION="v$AIMEM_VERSION" bash
RUN cd "$(mktemp -d)" \
    && base="https://github.com/BlackVS/aiskills/releases/download/v$AISKILLS_VERSION" \
    && curl -fsSLO "$base/aiskills-$AISKILLS_VERSION.tar.gz" && curl -fsSLO "$base/SHA256SUMS" \
    && sha256sum --ignore-missing -c SHA256SUMS \
    && mkdir src && tar -xzf "aiskills-$AISKILLS_VERSION.tar.gz" -C src --strip-components=1 \
    && bash ./src/install.sh --user -t claude -s "$AISKILLS"
ENTRYPOINT ["bash", "/usr/local/bin/member.sh"]
