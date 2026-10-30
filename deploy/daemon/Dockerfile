# The gastown daemon image: the town's own gt, bd and om on the base the polecat
# image uses, running `gt daemon run` as a container (gt-qpeei.16).
#
# Built by internal/daemonimage's Builder.BuildImage, which passes this file with
# -f, the directory of the binaries (gt, bd, om and claude) as the context, and
# the tag gt-daemon:<rev>. Nothing is compiled here: the deployer cross-compiles
# gt, bd and om on this Mac and fetches the pinned claude CLI, then COPYs them
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
# HTTPS calls verify against. These three are the only things installed.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git tmux \
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

# The claude CLI om's reviewer runs, at the version templates.ClaudeCLIVersion
# names. The deployer's FetchClaudeCLI downloads it, verifies its sha256 against
# templates.ClaudeCLISHA256, and places it in the build context; nothing here
# downloads it or runs an install script for it.
COPY --chmod=0755 claude /usr/local/bin/claude

# The scratch town's first start runs a dolt preflight that copies git's global
# user.name and user.email into dolt's config, and fails the install without them
# (internal/cmd/install.go). A host has a person's identity to copy; an image has
# none, so the image carries one. It is the daemon's, never a person's name or
# address, and because no USER instruction sets one, these write root's config:
# root is the user the container runs as.
RUN git config --global user.name gt-daemon
RUN git config --global user.email gt-daemon@gastown.local

# The entrypoint is a gt subcommand rather than a shell script: it refuses to
# start without a town on the bind mount, so a missing mount never turns into a
# fresh town. The subcommand is added by gt-qpeei.26; until it lands, gt rejects
# it and the container exits, so no container runs this image yet.
ENTRYPOINT ["gt", "daemon", "container-entrypoint"]
