"""The ProtoStrap sample's py service runs FastAPI on uvicorn, one process per replica.

ProtoStrap scales it out by requests per second (counted by the edge) and by CPU.
"""

import asyncio
import json
import logging
import os
import re
import socket
import sys
import threading
import time
from contextlib import asynccontextmanager
from datetime import datetime, timezone

import redis.asyncio as redis
import uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, PlainTextResponse
from redis.exceptions import RedisError

IDENTITY = {
    "app": os.environ.get("PROTOSTRAP_APP") or "sample",
    "component": os.environ.get("PROTOSTRAP_COMPONENT") or "py",
    "replica": os.environ.get("PROTOSTRAP_REPLICA") or socket.gethostname(),
    "release": os.environ.get("PROTOSTRAP_RELEASE") or "local",
}
PORT = 8000
DRAIN_SECONDS = float(os.environ.get("DRAIN_SECONDS") or 10)
MAX_BURN_MS = 5000
PORT_SUFFIX = re.compile(r":\d+$")

log = logging.getLogger("py")


class JsonFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        entry = {
            "time": datetime.fromtimestamp(record.created, timezone.utc).isoformat(),
            "level": record.levelname,
            "msg": record.getMessage(),
            "logger": record.name,
            "component": IDENTITY["component"],
            "replica": IDENTITY["replica"],
            "release": IDENTITY["release"],
        }
        if record.exc_info:
            entry["exception"] = self.formatException(record.exc_info)
        return json.dumps(entry)


def load_settings() -> dict[str, str]:
    """load_settings returns the component's keys when SECRET_ID is set.

    The environment's secret is a JSON object of key to string. The component's keys are
    those that start with its prefix, SECRET_PREFIX, and the Shared_ ones, without their
    prefixes, its own winning. Without SECRET_ID (a local run), the same keys come from the
    environment.
    """
    secret_id = os.environ.get("SECRET_ID")
    if not secret_id:
        return dict(os.environ)
    import boto3  # boto3 is only needed when running in ProtoStrap.

    # The default chain finds the agent's container credentials. botocore reads
    # AWS_DEFAULT_REGION but not AWS_REGION, which is what ProtoStrap sets.
    region = os.environ.get("AWS_REGION") or os.environ.get("AWS_DEFAULT_REGION")
    client = boto3.client("secretsmanager", region_name=region)
    values = json.loads(client.get_secret_value(SecretId=secret_id)["SecretString"])
    return component_keys(values, os.environ.get("SECRET_PREFIX"))


def component_keys(values: dict[str, str], prefix: str | None) -> dict[str, str]:
    """component_keys picks a component's keys out of the environment's secret.

    Without a prefix, from an agent older than the environment's secret, the secret is the
    component's own, and every key in it is.
    """
    if not prefix:
        return values
    keys = {k.removeprefix("Shared_"): v for k, v in values.items() if k.startswith("Shared_")}
    keys.update({k.removeprefix(prefix): v for k, v in values.items() if k.startswith(prefix)})
    return keys


def redis_address(connection: str) -> tuple[str, int, str | None]:
    """redis_address reads 'host:port,password=...', as in ConnectionStrings__redis.

    It returns the host, the port and the password, if any. Other options are ignored.
    """
    address, *options = [part.strip() for part in connection.split(",")]
    password = next((o.split("=", 1)[1] for o in options if o.startswith("password=")), None)
    host, _, port = address.rpartition(":")
    return (host, int(port), password) if host else (address, 6379, password)


class State:
    draining = False
    requests = 0
    loop_lag = 0.0


state = State()


async def watch_loop_lag(interval: float = 0.05, window: float = 5.0) -> None:
    """watch_loop_lag measures event loop lag, averaged over 5 s windows.

    The lag is how late a 50 ms sleep wakes.
    """
    loop = asyncio.get_running_loop()
    total, ticks, window_start = 0.0, 0, loop.time()
    while True:
        start = loop.time()
        await asyncio.sleep(interval)
        now = loop.time()
        total += max(0.0, now - start - interval)
        ticks += 1
        if now - window_start >= window:
            state.loop_lag = total / ticks
            total, ticks, window_start = 0.0, 0, now


@asynccontextmanager
async def lifespan(app: FastAPI):
    settings = load_settings()
    connection = settings.get("ConnectionStrings__redis")
    if connection:
        host, port, password = redis_address(connection)
        app.state.redis = redis.Redis(host=host, port=port, password=password, socket_connect_timeout=2, socket_timeout=2)
    else:
        log.warning("ConnectionStrings__redis is not set: /py/count is off")
        app.state.redis = None
    lag = asyncio.create_task(watch_loop_lag())
    log.info("started on port %d", PORT)
    yield
    lag.cancel()
    if app.state.redis is not None:
        await app.state.redis.aclose()


class CountRequests:
    """CountRequests counts requests, leaving out health checks and metrics."""

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope["type"] == "http" and scope["path"] not in ("/healthz", "/metrics"):
            state.requests += 1
        await self.app(scope, receive, send)


app = FastAPI(lifespan=lifespan, docs_url=None, redoc_url=None, openapi_url=None)
app.add_middleware(CountRequests)


@app.get("/healthz")
async def healthz(request: Request):
    # The replica is ready only when asked for this service by name, and not draining:
    # overlay addresses are reused by other services, so a check that reaches the wrong
    # replica must fail.
    host = PORT_SUFFIX.sub("", request.headers.get("host", ""))
    if state.draining or host != IDENTITY["component"]:
        return JSONResponse({"ready": False, "draining": state.draining}, status_code=503)
    return {"ready": True, **IDENTITY}


@app.get("/py/hello")
async def hello():
    return {**IDENTITY, "time": datetime.now(timezone.utc).isoformat()}


# burn spins until this thread has used ms of CPU time, so each request costs the same
# CPU. A plain def runs on a worker thread, so the event loop still answers health checks,
# but the GIL runs one burn at a time: a replica does at most 1000/ms of these a second,
# and elapsed_ms (wall-clock) grows once requests queue for it.
@app.get("/py/burn")
def burn(ms: int = 50):
    ms = max(0, min(ms, MAX_BURN_MS))
    start = time.perf_counter()
    until = time.thread_time() + ms / 1000
    while time.thread_time() < until:
        pass
    return {**IDENTITY, "elapsed_ms": round((time.perf_counter() - start) * 1000)}


@app.get("/py/count")
async def count(request: Request):
    client = request.app.state.redis
    if client is None:
        return JSONResponse({"error": "ConnectionStrings__redis is not set"}, status_code=503)
    try:
        value = await client.incr("py:count")
    except RedisError as err:
        log.warning("redis INCR failed: %s", err)
        return JSONResponse({"error": f"redis: {err}"}, status_code=503)
    return {**IDENTITY, "count": value}


@app.get("/metrics")
async def metrics():
    body = (
        "# HELP python_eventloop_lag_seconds Mean lateness of a 50 ms sleep over the last 5 s.\n"
        "# TYPE python_eventloop_lag_seconds gauge\n"
        f"python_eventloop_lag_seconds {state.loop_lag:.6f}\n"
        "# HELP py_requests_total Requests served, health checks and metrics left out.\n"
        "# TYPE py_requests_total counter\n"
        f"py_requests_total {state.requests}\n"
    )
    return PlainTextResponse(body, media_type="text/plain; version=0.0.4")


class DrainingServer(uvicorn.Server):
    """On SIGTERM, DrainingServer fails readiness at once and keeps serving for
    DRAIN_SECONDS, so the gateway notices, before uvicorn's graceful shutdown stops
    accepting, finishes requests and exits."""

    def handle_exit(self, sig, frame):
        if state.draining:  # A second signal stops the server now.
            self.should_exit = True
            return
        state.draining = True
        log.info("draining for %ss", DRAIN_SECONDS)
        timer = threading.Timer(DRAIN_SECONDS, lambda: setattr(self, "should_exit", True))
        timer.daemon = True
        timer.start()


def main() -> None:
    handler = logging.StreamHandler(sys.stdout)
    handler.setFormatter(JsonFormatter())
    logging.basicConfig(level=logging.INFO, handlers=[handler], force=True)
    config = uvicorn.Config(
        app,
        host="0.0.0.0",
        port=PORT,
        log_config=None,
        access_log=False,
        server_header=False,
        # Longer than Envoy keeps an idle connection (60 s): uvicorn's default, 5 s, closes
        # connections the edge is about to reuse, and requests fail at random.
        timeout_keep_alive=65,
        # The graceful shutdown follows draining and ends inside the spec's 20 s stop.
        timeout_graceful_shutdown=5,
    )
    DrainingServer(config).run()


if __name__ == "__main__":
    main()
