ARG BASE_IMAGE_REGISTRY_ARG=docker.io/library
FROM ${BASE_IMAGE_REGISTRY_ARG}/python:3.12-slim-bookworm
ARG PIP_INDEX_URL_ARG=https://pypi.org/simple
ARG UV_VERSION_ARG=0.11.32
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 UV_COMPILE_BYTECODE=1
ENV UV_DEFAULT_INDEX=${PIP_INDEX_URL_ARG}
COPY packages/uv/ /opt/preloaded-uv/
RUN UV_WHEEL=$(find /opt/preloaded-uv -maxdepth 1 -type f -name "uv-${UV_VERSION_ARG}-*.whl" -print -quit) \
    && test -n "$UV_WHEEL" \
    && python -m pip install --no-cache-dir --no-index "$UV_WHEEL" \
    && rm -rf /opt/preloaded-uv
WORKDIR /app
COPY custom/services/agent-runtime/pyproject.toml custom/services/agent-runtime/uv.lock ./
RUN uv sync --frozen --no-dev
COPY custom/services/agent-runtime/app ./app
COPY internal/custom/modules/usererrors/catalog.json ./app/public_error_catalog.json
ENV PATH="/app/.venv/bin:$PATH"
EXPOSE 8091
CMD ["uvicorn","app.server:app","--host","0.0.0.0","--port","8091","--no-access-log"]
