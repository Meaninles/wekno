FROM python:3.11-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN set -eux; \
    packages="libltdl7 libreoffice-writer-nogui libreoffice-calc-nogui libreoffice-draw-nogui pandoc poppler-utils qpdf ghostscript fontconfig fonts-noto-cjk fonts-wqy-zenhei fonts-wqy-microhei"; \
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

COPY custom/services/document-processing-agent/fonts-weknora.conf /etc/fonts/conf.d/64-weknora-document-fonts.conf
RUN fc-cache -f

WORKDIR /app
COPY custom/services/document-processing-agent/requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt

COPY custom/services/general-agent/app /app/app

EXPOSE 8091
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8091"]
