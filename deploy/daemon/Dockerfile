# The gastown daemon image: the town's own gt, bd and om on the base the polecat
# image uses, running `gt daemon run` as a container (gt-qpeei.16).
#
# Built by internal/daemonimage's Builder.BuildImage, which passes this file with
# -f, the directory of the binaries as the context, and the tag gt-daemon:<rev>.
# Nothing is compiled here: the deployer cross-compiles gt, bd and om on this Mac
# and fetches the pinned claude CLI and the pinned client tools, then COPYs them
# in, so the image is exactly the code the tag's rev names.
#
# The base image and the Dolt version are internal/templates/images.go's
# DaemonBaseImage and DoltVersion. TestDaemonDockerfileMatchesTheImageTable fails
# when this file and those constants disagree.

# The Dolt client, at the version the town's Dolt server runs, taken from the
# official Dolt image and copied into the image below. Nothing fetches it over
# the network here: a registry pull verifies every layer's digest, which a
# tarball downloaded inside a RUN would not have unless a sha256 were written out
# beside the version and kept in step with it.
ARG DOLT_VERSION=2.3.2
ARG DOLT_DIGEST=sha256:9a077bc9c8cb6604a6d458c734096b482735f640144e5665b36161d560f14861
FROM dolthub/dolt:${DOLT_VERSION}@${DOLT_DIGEST} AS dolt

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# git and tmux are what the daemon's patrols run; ca-certificates is what its
# HTTPS calls verify against; openssh-client is the ssh git runs for a push over
# SSH, which app-promote does to GitHub with the promote keys. These four are the
# only things installed.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git openssh-client tmux \
    && rm -rf /var/lib/apt/lists/*

# The Dolt client, from the image above. It is a client only: the town's data
# stays on the Mac and the daemon reaches it over the socket, so no data
# directory is ever in this image.
COPY --from=dolt /usr/local/bin/dolt /usr/local/bin/dolt

# The town's binaries, cross-compiled at the rev the image is tagged with. The
# COPY sources are entries of the build context, which BuildImage sets to the
# output directory BuildBinaries wrote them to.
COPY --chmod=0755 gt /usr/local/bin/gt
COPY --chmod=0755 bd /usr/local/bin/bd
COPY --chmod=0755 om /usr/local/bin/om

# The plugins the container daemon runs itself, cross-compiled for linux/arm64
# from the same rev as the binaries above: the town's plugin directories hold
# macOS builds the container cannot exec. polecat-image, tool-updater and hm-sync
# run only on the host, so they are not shipped here.
COPY --chmod=0755 plugins/app-promote/app-promote /usr/local/lib/gt/plugins/app-promote/app-promote
COPY --chmod=0755 plugins/town-metrics/town-metrics /usr/local/lib/gt/plugins/town-metrics/town-metrics

# The claude CLI om's reviewer runs, at the version templates.ClaudeCLIVersion
# names. The deployer's FetchClaudeCLI downloads it, verifies its sha256 against
# templates.ClaudeCLISHA256, and places it in the build context; nothing here
# downloads it or runs an install script for it.
COPY --chmod=0755 claude /usr/local/bin/claude

# The client tools the daemon execs for dispatch (openshell, kubectl) and for the
# tier sweep (docker, to the sweep engine through DOCKER_HOST): pinned linux-arm64
# downloads the deployer verifies by sha256 (templates.DaemonClientTools) and
# places in the build context. Nothing here downloads them.
COPY --chmod=0755 openshell /usr/local/bin/openshell
COPY --chmod=0755 docker /usr/local/bin/docker
COPY --chmod=0755 kubectl /usr/local/bin/kubectl

# The scratch town's first start runs a dolt preflight that copies git's global
# user.name and user.email into dolt's config, and fails the install without them
# (internal/cmd/install.go). A host has a person's identity to copy; an image has
# none, so the image carries one. It is the daemon's, never a person's name or
# address. Each RUN writes the config of the user it builds as, which is root
# while no USER instruction is set; the container runs as that same user, so a
# USER added later would leave the identity in a home the container does not read.
# TestDaemonDockerfileRunsAsRootWhereItsIdentityIs fails on any USER instruction.
RUN git config --global user.name gt-daemon
RUN git config --global user.email gt-daemon@gastown.local

# The entrypoint is a gt subcommand rather than a shell script: it refuses to
# start without a town on the bind mount, so a missing mount never turns into a
# fresh town. The subcommand is added by gt-qpeei.26; until it lands, gt rejects
# it and the container exits, so no container runs this image yet.
ENTRYPOINT ["gt", "daemon", "container-entrypoint"]
