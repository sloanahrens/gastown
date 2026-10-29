# The gastown daemon image: the town's own gt, bd and om on the base the polecat
# image uses, running `gt daemon run` as a container (gt-qpeei.16).
#
# Built by internal/daemonimage's Builder.BuildImage, which passes this file with
# -f, the directory of the three binaries as the context, and the tag
# gt-daemon:<rev>. Nothing is compiled here: the deployer cross-compiles the
# binaries on this Mac and COPYs them in, so the image is exactly the code the
# tag's rev names.
#
# The base image and the Dolt version are internal/templates/images.go's
# DaemonBaseImage and DoltVersion. TestDaemonDockerfileMatchesTheImageTable fails
# when this file and those constants disagree.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# git and tmux are what the daemon's patrols run; ca-certificates and curl serve
# the Dolt download below and the daemon's own HTTPS calls.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git tmux \
    && rm -rf /var/lib/apt/lists/*

# The Dolt client, at the version the town's Dolt server runs. It is a client
# only: the town's data stays on the Mac and the daemon reaches it over the
# socket, so no data directory is ever in this image.
ARG DOLT_VERSION=2.3.2
# arm64 alone, because BuildImage's binaries are linux/arm64 and the Docker
# Desktop VM on this Mac is the one the design runs in.
RUN curl -fsSL -o /tmp/dolt.tar.gz \
        "https://github.com/dolthub/dolt/releases/download/v${DOLT_VERSION}/dolt-linux-arm64.tar.gz" \
    && tar -xzf /tmp/dolt.tar.gz -C /tmp \
    && install -m 0755 /tmp/dolt-linux-arm64/bin/dolt /usr/local/bin/dolt \
    && rm -rf /tmp/dolt.tar.gz /tmp/dolt-linux-arm64 \
    && dolt version

# The town's binaries, cross-compiled at the rev the image is tagged with. The
# COPY sources are entries of the build context, which BuildImage sets to the
# output directory BuildBinaries wrote them to.
COPY gt /usr/local/bin/gt
COPY bd /usr/local/bin/bd
COPY om /usr/local/bin/om
RUN chmod 0755 /usr/local/bin/gt /usr/local/bin/bd /usr/local/bin/om

# The entrypoint is a gt subcommand rather than a shell script: it refuses to
# start without a town on the bind mount, so a missing mount never turns into a
# fresh town (D5).
ENTRYPOINT ["gt", "daemon", "container-entrypoint"]
