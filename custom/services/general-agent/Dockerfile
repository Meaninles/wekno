ARG BASE_IMAGE_REGISTRY_ARG=docker.io/library
FROM ${BASE_IMAGE_REGISTRY_ARG}/python:3.11-slim AS python-wheels

WORKDIR /wheels
ARG PIP_INDEX_URL_ARG=https://mirrors.tencent.com/pypi/simple
COPY custom/services/general-agent/requirements.txt /tmp/requirements.txt
RUN pip wheel --index-url "$PIP_INDEX_URL_ARG" --wheel-dir /wheels -r /tmp/requirements.txt

FROM ${BASE_IMAGE_REGISTRY_ARG}/python:3.11-slim

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    NODE_PATH=/usr/local/lib/node_modules

ARG APT_MIRROR_ARG=mirrors.tencent.com
ARG NPM_REGISTRY_ARG=https://mirrors.tencent.com/npm

RUN set -eux; \
    if [ -n "$APT_MIRROR_ARG" ]; then \
        sed -i "s@deb.debian.org@${APT_MIRROR_ARG}@g" /etc/apt/sources.list.d/debian.sources; \
    fi; \
    packages="ca-certificates curl wget git zip unzip jq file procps nodejs npm libltdl7 libreoffice-writer-nogui libreoffice-calc-nogui libreoffice-draw-nogui pandoc poppler-utils qpdf ghostscript fontconfig fonts-noto-cjk fonts-wqy-zenhei fonts-wqy-microhei"; \
    for attempt in 1 2 3 4 5; do \
        apt-get -o Acquire::Retries=5 update; \
        if apt-get -o Acquire::Retries=5 install -y --no-install-recommends $packages; then \
            break; \
        fi; \
        apt-get -o Acquire::Retries=5 -f install -y --no-install-recommends || true; \
        if apt-get -o Acquire::Retries=5 install -y --no-install-recommends $packages; then \
            break; \
        fi; \
        if [ "$attempt" = "5" ]; then exit 1; fi; \
        apt-get clean; \
        rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/partial/*; \
        sleep 10; \
    done; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

RUN npm config set registry "$NPM_REGISTRY_ARG" && \
    npm install -g pptxgenjs && npm cache clean --force

WORKDIR /app
COPY custom/services/general-agent/requirements.txt /app/requirements.txt
COPY --from=python-wheels /wheels /tmp/wheels
RUN pip install --no-cache-dir --no-index --find-links=/tmp/wheels -r /app/requirements.txt && \
    rm -rf /tmp/wheels
COPY custom/services/general-agent/app /app/app
COPY custom/services/document-processing-agent/fonts-weknora.conf /etc/fonts/conf.d/64-weknora-document-fonts.conf
RUN fc-cache -f

EXPOSE 8091
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8091"]
