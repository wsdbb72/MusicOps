# syntax=docker/dockerfile:1
# MusicOps Linux amd64 container: Go core only, no Python runtime.
#
# Build stage: Go toolchain plus the Python *build tools* that
# packaging/prepare_engines.py needs (a venv + PyInstaller to freeze
# FlareSolverr). That Python is a build-time tool only: nothing from it
# is copied into the runtime stage, and the runtime image contains no
# Python interpreter and no .py files.
#
# The one exception is the FlareSolverr engine itself: it is a
# third-party PyInstaller-frozen binary produced by prepare_engines.py
# and shipped under /opt/musicops/tools/services/flaresolverr/. It is not
# MusicOps code. The no-Python scan (native/core/internal/smoke.
# CheckNoPython) excludes the services directory for exactly this
# reason, and the engines check verifies the frozen binaries separately.

ARG VERSION=0.0.0-dev
ARG REVISION=unknown

FROM golang:1.26.3-bookworm AS build
ARG VERSION
ENV DEBIAN_FRONTEND=noninteractive
# git clones the pinned engine sources; python3-venv builds the
# FlareSolverr freeze; chromium/chromedriver are what prepare_engines.py
# links into the Linux engine output (a symlink to /usr/bin/chromium and
# a copy of /usr/bin/chromedriver). No gcc: every Go build below sets
# CGO_ENABLED=0, including the go-music-dl build inside
# prepare_engines.py.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates chromium chromium-driver git python3 python3-venv \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /build
# Go module cache first, so dependency downloads are cached across
# source edits; then the full core source.
COPY native/core/go.mod native/core/go.sum ./native/core/
RUN cd native/core && go mod download
COPY native/core ./native/core
COPY packaging ./packaging
# collect_licenses.py (run by prepare_engines.py) copies the PlaylistOut
# license out of docs/references when present.
COPY docs/references ./docs/references
# Managed engines: go-music-dl + FlareSolverr (+ engines.json and
# licenses). Needs network: engine sources are cloned from GitHub at
# pinned revisions, and the FlareSolverr requirements are pip-installed
# into a throwaway venv. Output layout is <output>/go-music-dl,
# <output>/flaresolverr, <output>/licenses, <output>/engines.json.
RUN python3 packaging/prepare_engines.py --output /engines --cache /engine-build
# Go core + release gate + container smoke tool. The core and
# docker-smoke link internal/version, so they get the -X injection;
# releasegate does not, and -X on a symbol that is not in the binary
# fails the build, so releasegate is built without it.
WORKDIR /build/native/core
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-X github.com/wsdbb72/MusicOps/native/core/internal/version.Version=${VERSION}" \
      -o /out/musicops ./cmd/musicops \
 && CGO_ENABLED=0 go build -trimpath -o /out/releasegate ./cmd/releasegate \
 && CGO_ENABLED=0 go build -trimpath \
      -ldflags "-X github.com/wsdbb72/MusicOps/native/core/internal/version.Version=${VERSION}" \
      -o /out/docker-smoke ./cmd/docker-smoke

FROM debian:bookworm-slim AS runtime
ARG VERSION
ARG REVISION
LABEL org.opencontainers.image.title="MusicOps" \
      org.opencontainers.image.source="https://github.com/wsdbb72/MusicOps" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
ENV DEBIAN_FRONTEND=noninteractive
# chromium/chromedriver are runtime needs too: the engine output links
# /usr/bin/chromium (symlink) and carries a copy of /usr/bin/chromedriver,
# and FlareSolverr drives the distribution browser at runtime. ffmpeg /
# ffprobe serve the transcode executor (resolved via exec.LookPath).
# xvfb stays for the browser engine; tini is PID 1; openssl generates
# the first-boot web password in the entrypoint.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates chromium chromium-driver ffmpeg fonts-noto-cjk \
    openssl tini xvfb \
    && rm -rf /var/lib/apt/lists/*
# Bundle layout (the releasegate "docker" layout): core at the bundle
# root, engines at tools/services, licenses at the bundle root.
# releasegate and docker-smoke ship in the bundle so the release workflow
# can extract them from the tested image and run them on the host
# (docker-smoke drives the host docker CLI; it is not an in-container
# entrypoint). They are Go binaries, not Python.
COPY --from=build /out/musicops /opt/musicops/musicops
COPY --from=build /out/releasegate /opt/musicops/releasegate
COPY --from=build /out/docker-smoke /opt/musicops/docker-smoke
COPY --from=build /engines/go-music-dl /opt/musicops/tools/services/go-music-dl
COPY --from=build /engines/flaresolverr /opt/musicops/tools/services/flaresolverr
COPY --from=build /engines/engines.json /opt/musicops/tools/services/engines.json
COPY --from=build /engines/licenses /opt/musicops/licenses
COPY LICENSE /opt/musicops/LICENSE
COPY packaging/THIRD-PARTY-NOTICES.md /opt/musicops/THIRD-PARTY-NOTICES.md
COPY docker/entrypoint.sh /opt/musicops/entrypoint.sh
RUN chmod 0755 /opt/musicops/entrypoint.sh /opt/musicops/musicops /opt/musicops/releasegate /opt/musicops/docker-smoke \
 && useradd --create-home --uid 1000 musicops \
 && mkdir -p /config /media/music /media/MusicOps \
 && chown -R 1000:1000 /config /media /opt/musicops
# PATH carries /opt/musicops so the HEALTHCHECK below resolves the
# `musicops` binary without the entrypoint, and /opt/musicops/tools so
# the core finds bundled tools via exec.LookPath.
ENV MUSICOPS_VAULT_KEY_FILE=/config/keys/vault.key \
    HOME=/config/home \
    XDG_DATA_HOME=/config/home/.local/share \
    PATH="/opt/musicops:/opt/musicops/tools:${PATH}"
USER 1000:1000
WORKDIR /config
EXPOSE 8080
VOLUME ["/config", "/media"]
HEALTHCHECK --interval=30s --timeout=15s --start-period=600s --retries=3 \
  CMD ["musicops", "healthcheck", "--addr", "127.0.0.1:8080"]
ENTRYPOINT ["/usr/bin/tini", "-s", "--", "/opt/musicops/entrypoint.sh"]
