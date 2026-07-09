FROM python:3.12-slim

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app ./app
COPY run.py pyproject.toml README.md ./
COPY config.example.json ./

RUN mkdir -p /app/data

ENV GROK2API_HOST=0.0.0.0 \
    GROK2API_PORT=8787 \
    GROK2API_DATA_DIR=/app/data \
    GROK2API_MODE=cli \
    PYTHONUNBUFFERED=1

EXPOSE 8787

CMD ["python", "run.py"]
