FROM python:3.12-slim

ARG TARGETARCH
ARG VERSION=v20260306.0257
ARG REPO=PEroky/pyrunner-src

RUN apt-get update && apt-get install -y --no-install-recommends \
    tini curl && \
    rm -rf /var/lib/apt/lists/*

RUN ARCH=$(case "${TARGETARCH}" in amd64) echo "linux-amd64";; arm64) echo "linux-arm64";; *) echo "linux-${TARGETARCH}";; esac) && \
    curl -fSL "https://github.com/${REPO}/releases/download/${VERSION}/pyrunner-${VERSION}-${ARCH}" -o /usr/local/bin/pyrunner && \
    chmod +x /usr/local/bin/pyrunner

WORKDIR /app
RUN mkdir -p /data/scripts

ENV PYRUNNER_PORT=8000 \
    PYRUNNER_USER= \
    PYRUNNER_PASS= \
    PYRUNNER_DATA=/data

EXPOSE 8000
VOLUME /data
ENTRYPOINT ["tini", "--"]
CMD ["pyrunner"]
