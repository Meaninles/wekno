FROM ghcr.io/astral-sh/uv:0.10.4 AS uv
FROM python:3.12-slim-bookworm
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 UV_COMPILE_BYTECODE=1
COPY --from=uv /uv /usr/local/bin/uv
WORKDIR /app
COPY custom/services/agent-runtime/pyproject.toml custom/services/agent-runtime/uv.lock ./
RUN uv sync --frozen --no-dev
COPY custom/services/agent-runtime/app ./app
COPY internal/custom/modules/usererrors/catalog.json ./app/public_error_catalog.json
ENV PATH="/app/.venv/bin:$PATH"
EXPOSE 8091
CMD ["uvicorn","app.server:app","--host","0.0.0.0","--port","8091","--no-access-log"]
