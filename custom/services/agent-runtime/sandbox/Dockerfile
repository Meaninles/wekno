FROM python:3.12-slim-bookworm
ENV DEBIAN_FRONTEND=noninteractive PYTHONUNBUFFERED=1 NODE_PATH=/usr/local/lib/node_modules
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates git zip unzip jq file procps nodejs npm ripgrep \
    libreoffice-writer-nogui libreoffice-calc-nogui libreoffice-draw-nogui libreoffice-impress-nogui \
    pandoc poppler-utils qpdf ghostscript fontconfig fonts-noto-cjk \
    && rm -rf /var/lib/apt/lists/*
COPY custom/services/agent-runtime/sandbox/requirements.txt /opt/agent/requirements.txt
RUN pip install --no-cache-dir -r /opt/agent/requirements.txt && npm install -g pptxgenjs@4.0.1 && npm cache clean --force
RUN groupadd -g 1000 agent && useradd -u 1000 -g 1000 -d /workspace agent \
    && mkdir /workspace /control && chown 1000:1000 /workspace && chmod 700 /control
COPY custom/services/agent-runtime/sandbox/job.py /opt/agent/job.py
COPY custom/services/agent-runtime/sandbox/files.py /opt/agent/files.py
COPY custom/services/agent-runtime/sandbox/fonts-weknora.conf /etc/fonts/conf.d/99-weknora.conf
WORKDIR /workspace
CMD ["sleep","infinity"]
