# syntax=docker/dockerfile:1
FROM python:3.12-slim-bookworm AS system
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    chromium chromium-driver ffmpeg fonts-noto-cjk xvfb \
    openssl ca-certificates tini && rm -rf /var/lib/apt/lists/*
FROM golang:1.26.3-bookworm AS golang
FROM system AS build
COPY --from=golang /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
RUN apt-get update && apt-get install -y --no-install-recommends git gcc libc6-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /build
COPY docker/requirements.txt /build/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt
COPY packaging /build/packaging
COPY docs/references /build/docs/references
RUN python packaging/prepare_engines.py --output /engines --cache /engine-build
COPY pyproject.toml /build/
COPY src /build/src
RUN pip wheel --no-deps --wheel-dir /wheels . && python packaging/collect_licenses.py /licenses
FROM system AS runtime
ARG VERSION=0.4.22
ARG REVISION=unknown
LABEL org.opencontainers.image.title="MusicOps" \
      org.opencontainers.image.source="https://github.com/wsdbb72/MusicOps" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY docker/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt && rm /tmp/requirements.txt
COPY --from=build /wheels /tmp/wheels
RUN pip install --no-deps /tmp/wheels/*.whl && rm -r /tmp/wheels
COPY --from=build /engines /opt/musicops/engines
COPY --from=build /licenses /opt/musicops/licenses
COPY packaging /opt/musicops/packaging
COPY docker /opt/musicops/docker
RUN useradd --create-home --uid 1000 musicops && mkdir -p /config /media/music /media/MusicOps && chown -R 1000:1000 /config /media /opt/musicops
ENV MUSICOPS_CONTAINER=1 MUSICOPS_ENGINE_ROOT=/opt/musicops/engines \
    MUSICOPS_VAULT_KEY_FILE=/config/keys/vault.key \
    HOME=/config/home XDG_DATA_HOME=/config/home/.local/share \
    QT_QPA_PLATFORM=offscreen \
    PYTHONUNBUFFERED=1
USER 1000:1000
WORKDIR /config
EXPOSE 8080
VOLUME ["/config", "/media"]
HEALTHCHECK --interval=30s --timeout=15s --start-period=120s --retries=3 \
 CMD ["python", "/opt/musicops/docker/healthcheck.py"]
ENTRYPOINT ["/usr/bin/tini", "-s", "--", "python", "/opt/musicops/docker/entrypoint.py"]
