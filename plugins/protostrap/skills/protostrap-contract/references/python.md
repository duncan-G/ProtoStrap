# Python under ProtoStrap

A working FastAPI service is ProtoStrap's `sample/services/py/main.py`. One uvicorn process
holds the GIL, so a component scales out on requests per second and CPU, with a request of a
core or less.

## Keys

```python
import json, os

def load_settings() -> dict[str, str]:
    """The component's keys: from the environment's secret when SECRET_ID is set, else the
    environment, as in a local run."""
    secret_id = os.environ.get("SECRET_ID")
    if not secret_id:
        return dict(os.environ)
    import boto3
    # botocore reads AWS_DEFAULT_REGION, not AWS_REGION, which is what ProtoStrap sets.
    region = os.environ.get("AWS_REGION") or os.environ.get("AWS_DEFAULT_REGION")
    client = boto3.client("secretsmanager", region_name=region)
    values = json.loads(client.get_secret_value(SecretId=secret_id)["SecretString"])
    prefix = os.environ.get("SECRET_PREFIX")
    keys = {k.removeprefix("Shared_"): v for k, v in values.items() if k.startswith("Shared_")}
    if prefix:
        keys.update({k.removeprefix(prefix): v for k, v in values.items() if k.startswith(prefix)})
    return keys  # Its own keys win.
```

Pass `region_name` to every boto3 client the same way.

## Readiness and draining

```python
import threading, uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

COMPONENT = os.environ.get("PROTOSTRAP_COMPONENT")
DRAIN_SECONDS = float(os.environ.get("DRAIN_SECONDS") or 10)
draining = False
app = FastAPI()

@app.get("/healthz")
def healthz(request: Request):
    # Ready only when asked for this service by name, and not draining.
    host = request.headers.get("host", "").rsplit(":", 1)[0]
    if draining or (COMPONENT and host != COMPONENT):
        return JSONResponse({"ready": False}, status_code=503)
    return {"ready": True}

class DrainingServer(uvicorn.Server):
    """On SIGTERM, fail readiness at once and keep serving for DRAIN_SECONDS, then let
    uvicorn's graceful shutdown finish requests and exit."""
    def handle_exit(self, sig, frame):
        global draining
        if draining:  # A second signal stops now.
            self.should_exit = True
            return
        draining = True
        timer = threading.Timer(DRAIN_SECONDS, lambda: setattr(self, "should_exit", True))
        timer.daemon = True
        timer.start()

if __name__ == "__main__":
    DrainingServer(uvicorn.Config(
        app, host="0.0.0.0", port=8000, log_config=None, access_log=False,
        timeout_keep_alive=65,        # Longer than Envoy's 60 s idle timeout; the default is 5.
        timeout_graceful_shutdown=5,  # DRAIN_SECONDS + this, inside the spec's stop.
    )).run()
```

Run it as `python main.py`, not `uvicorn main:app`, so the subclass handles the signal. One
process per replica: scale with replicas, not `--workers`.

## Other services, queues and buckets

- Call others by name (`http://api:8080`) with an `httpx.AsyncClient` whose connections are
  recycled about every minute, retrying a refused connection.
- `QUEUE_<NAME>_URL`, `BUCKET_<NAME>` and `FILES_BUCKET` from `os.environ`. Against
  LocalStack, pass `endpoint_url=os.environ.get("AWS_ENDPOINT_URL_S3")` and
  `config=Config(s3={"addressing_style": "path"})` (recent botocore reads the endpoint
  variables itself).
- A worker loop: `receive_message(WaitTimeSeconds=20)`, do the job idempotently,
  `delete_message` after; on SIGTERM stop receiving and `change_message_visibility(…, VisibilityTimeout=0)`
  for messages not started.

## Logs

A `logging.Formatter` that writes one JSON object per line to stdout, with the component,
replica and release; `access_log=False` under load.

Logs stay on stdout; don't also export them over OTLP. Traces, metrics and trace IDs in
each line: protostrap-telemetry's `references/python.md`.
