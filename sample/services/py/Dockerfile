FROM python:3.13-slim-bookworm
ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1
WORKDIR /app
COPY requirements.txt ./
RUN pip install -r requirements.txt
COPY main.py ./
RUN useradd --system --no-create-home --uid 10001 app
USER 10001
# Port 8000 serves the app and /metrics.
EXPOSE 8000
CMD ["python", "main.py"]
