# The daemon image's mango, built from a mango checkout (the build context) with
# a plain docker build. The images workflow exports the one file this image
# holds with --output, so nothing bind-mounts a host path: a docker engine that
# runs beside the job cannot see the runner's directories (gt-t2jlo9).
#
# Both RUN lines are internal/daemonimage's mangoAptInstall and mangoGoBuild,
# the commands `gt daemon image` runs on a developer machine; TestMangoDockerfile
# holds them equal. mango's go-api links DuckDB through cgo, so it is compiled
# on linux in a container rather than cross-compiled.
FROM golang:1.27-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends gcc g++
COPY . /src
WORKDIR /src/go-api
RUN CGO_ENABLED=1 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-w -s" -o /out/mango ./cmd/mango

FROM scratch
COPY --from=build /out/mango /mango
