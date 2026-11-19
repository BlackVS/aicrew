# A member home (sim-coordinator, sim-worker, sim-indep): git and Python 3,
# the client stand-in as `claude` on PATH, b4's aimem-timed wrapper that
# the home joins with as its aimem command, then each component by its own
# installer (DESIGN-PILOT-SIMULATION section 3): aicrew's boot.sh from this
# tree's binaries, or in the upgrade scenario the previous release's own
# boot.sh from its export (AICREW_BASE, every file checked against its
# SHA256SUMS); aimem's verifying boot script taken at the tested tag,
# as aicrew-agent check instructs. A member of this tree's build has no
# ai-skills before join: join installs them from the hub inside the team's
# conversation, as on a person's hosts (task 01a1f3a6-77d3). Only the
# upgrade scenario's previous release, which has no such path, gets them
# from the ai-skills release archive checked against SHA256SUMS, as that
# release's homes had them before the upgrade. aimem's installer needs jq.
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
COPY export/ /opt/aicrew-export/
COPY boot.sh /opt/aicrew-boot.sh
COPY member.sh /usr/local/bin/member.sh
COPY aimem-timed /usr/local/bin/aimem-timed
RUN chmod 755 /usr/local/bin/aimem-timed
USER member
WORKDIR /home/member
ENV PATH=/home/member/.local/bin:/usr/local/bin:/usr/bin:/bin
RUN if [ -f /opt/aicrew-export/MANIFEST ]; then       tag=$(sed -n 's/^aicrew //p' /opt/aicrew-export/MANIFEST)       && AICREW_BASE=/opt/aicrew-export AICREW_NO_SERVICE=1 bash "/opt/aicrew-export/aicrew/$tag/boot.sh";     else AICREW_PREBUILT_DIR=/opt/aicrew-prebuilt AICREW_NO_SERVICE=1 bash /opt/aicrew-boot.sh; fi
RUN cd "$(mktemp -d)" && curl -fsSL "https://raw.githubusercontent.com/BlackVS/aimem/v$AIMEM_VERSION/boot.sh" \
    | AIMEM_VERSION="v$AIMEM_VERSION" bash
RUN if [ -f /opt/aicrew-export/MANIFEST ]; then cd "$(mktemp -d)" \
    && base="https://github.com/BlackVS/aiskills/releases/download/v$AISKILLS_VERSION" \
    && curl -fsSLO "$base/aiskills-$AISKILLS_VERSION.tar.gz" && curl -fsSLO "$base/SHA256SUMS" \
    && sha256sum --ignore-missing -c SHA256SUMS \
    && mkdir src && tar -xzf "aiskills-$AISKILLS_VERSION.tar.gz" -C src --strip-components=1 \
    && bash ./src/install.sh --user -t claude -s "$AISKILLS"; fi
ENTRYPOINT ["bash", "/usr/local/bin/member.sh"]
