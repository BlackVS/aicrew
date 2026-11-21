# sim-aicrewd: aicrewd installed by install-aicrewd.sh in its install-only
# mode from this tree's binaries (prebuilt/), or in the upgrade scenario
# from the previous release's export (export/), the other left empty
# (DESIGN-PILOT-SIMULATION section 3); the container is its supervisor.
#
# The operator logs in to aicrewd's host by SSH and becomes the service user
# with sudo (docs/BRING-UP.md): `ops` is that login, with passwordless
# sudo and key logins only, for the carrying of the architect's credential
# (step 8). sim-architect, the architect's machine, runs this image too and
# uses its ssh client; only sim-aicrewd starts sshd, on the compose network.
# A member's image has none of this.
FROM debian:bookworm-20261020-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
      openssh-server openssh-client sudo \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 2002 -s /bin/bash sessiond \
    && useradd -m -u 2003 -s /bin/bash -p '*' ops \
    && echo 'ops ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/ops && chmod 440 /etc/sudoers.d/ops \
    && printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin no\nAllowUsers ops\n' \
      > /etc/ssh/sshd_config.d/sim.conf \
    && rm -f /etc/ssh/ssh_host_*
COPY prebuilt/ /opt/aicrew-prebuilt/
COPY export/ /opt/aicrew-export/
COPY install-aicrewd.sh /opt/install-aicrewd.sh
COPY aicrewd.sh /usr/local/sbin/aicrewd.sh
EXPOSE 9443
ENTRYPOINT ["bash", "/usr/local/sbin/aicrewd.sh"]
