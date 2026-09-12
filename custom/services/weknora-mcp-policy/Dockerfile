FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PYTHONPATH=/app

WORKDIR /app

COPY custom/services/weknora-mcp-policy/requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r /app/requirements.txt

COPY custom/services/weknora-mcp-policy/app /app/app
COPY custom/services/weknora-mcp-policy/config /app/config

EXPOSE 8000

ENTRYPOINT ["python", "-m", "app.server", "--transport", "http"]

