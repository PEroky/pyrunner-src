FROM python:3.12-slim

ARG TARGETARCH
ARG VERSION=v20260306.0257
ARG REPO=PEroky/pyrunner-src

RUN apt-get update && apt-get install -y --no-install-recommends \
    tini curl unzip && \
    rm -rf /var/lib/apt/lists/*

RUN ARCH=$(case "${TARGETARCH}" in amd64) echo "linux-amd64";; arm64) echo "linux-arm64";; *) echo "linux-${TARGETARCH}";; esac) && \
    curl -fSL "https://github.com/${REPO}/releases/download/${VERSION}/pyrunner-${VERSION}-${ARCH}" -o /usr/local/bin/pyrunner && \
    chmod +x /usr/local/bin/pyrunner

# 安装 xray
RUN XRAY_ARCH=$(case "${TARGETARCH}" in amd64) echo "64";; arm64) echo "arm64-v8a";; *) echo "${TARGETARCH}";; esac) && \
    curl -fSL "https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-${XRAY_ARCH}.zip" -o /tmp/xray.zip && \
    unzip /tmp/xray.zip xray -d /usr/local/bin && \
    chmod +x /usr/local/bin/xray && \
    rm /tmp/xray.zip

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
