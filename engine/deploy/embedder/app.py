"""Tiny HTTP embedding service for the engine's semantic dedup path.

The engine's internal/idem/semantic.HTTPEmbedder speaks one contract:

    POST /embed   {"text": "..."}   ->   {"embedding": [384 floats]}

This process is the other end of that contract. It exists only so the
project's semantic-duplicate check runs against a real sentence encoder
(sentence-transformers/all-MiniLM-L6-v2) instead of the engine's built-in
FakeEmbedder, which hashes text and so cannot tell two different phrasings
of the same request apart.

It is deliberately plain-library HTTP (no Flask/FastAPI): one JSON endpoint
does not need a web framework, and sentence-transformers + torch already
account for essentially all of this image's size and startup time.

GET /healthz returns 200 once the model has finished loading and 503 before
that — model load takes tens of seconds on a CPU, and the compose healthcheck
is written to expect exactly this shape (see deploy/docker-compose.yml).
"""

from __future__ import annotations

import json
import logging
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODEL_NAME = "all-MiniLM-L6-v2"
EMBEDDING_DIM = 384
HOST = "0.0.0.0"
PORT = 8000

logging.basicConfig(level=logging.INFO, format="%(asctime)s embedder: %(message)s")
log = logging.getLogger("embedder")

# _model is written once, by the loader thread, then only ever read. _ready
# is what every request checks before touching it, so there is no window
# where a request sees a half-initialised model object.
_model = None
_ready = threading.Event()


def _load_model() -> None:
    global _model
    # Imported here, not at module scope, so `python3 -m py_compile` on this
    # file (and any tooling that only wants to check syntax) does not require
    # torch/sentence-transformers to be installed.
    from sentence_transformers import SentenceTransformer

    log.info("loading %s ...", MODEL_NAME)
    model = SentenceTransformer(MODEL_NAME)
    # A throwaway encode warms up torch's internal thread pools and any
    # lazy-initialised kernels, so the first real request is not the slowest
    # one the service ever serves.
    model.encode("warmup", normalize_embeddings=True)
    _model = model
    _ready.set()
    log.info("%s loaded (dim=%d) — ready to serve", MODEL_NAME, EMBEDDING_DIM)


class Handler(BaseHTTPRequestHandler):
    server_version = "durexec-embedder/1.0"

    def _write_json(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802 (BaseHTTPRequestHandler's naming)
        if self.path == "/healthz":
            if _ready.is_set():
                self._write_json(200, {"status": "ready", "model": MODEL_NAME})
            else:
                self._write_json(503, {"status": "loading", "model": MODEL_NAME})
            return
        self._write_json(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if self.path != "/embed":
            self._write_json(404, {"error": "not found"})
            return
        if not _ready.is_set():
            self._write_json(503, {"error": "model is still loading, try again shortly"})
            return

        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            data = json.loads(raw or b"{}")
            text = data["text"]
            if not isinstance(text, str):
                raise TypeError("'text' must be a string")
        except (json.JSONDecodeError, KeyError, TypeError) as exc:
            self._write_json(400, {"error": f"bad request: {exc}"})
            return

        # normalize_embeddings=True unit-normalises the vector, matching how
        # the engine's FakeEmbedder and semantic.Cosine both expect vectors
        # to arrive (see internal/idem/semantic/embedder.go and cosine.go),
        # so cosine similarity reduces to a plain dot product either way.
        vector = _model.encode(text, normalize_embeddings=True)
        self._write_json(200, {"embedding": [float(x) for x in vector]})

    def log_message(self, fmt: str, *args) -> None:  # noqa: A003
        log.info("%s - %s", self.address_string(), fmt % args)


def main() -> None:
    # The server starts listening immediately, on a background thread the
    # model loads in parallel. That is what makes /healthz meaningful during
    # the tens of seconds the model takes to load: without this, the compose
    # healthcheck would just see "connection refused" the whole time instead
    # of a clean 503, and requests to /embed would hang instead of failing
    # fast with a clear reason.
    threading.Thread(target=_load_model, name="model-loader", daemon=True).start()

    httpd = ThreadingHTTPServer((HOST, PORT), Handler)
    log.info("listening on %s:%d (model loading in the background)", HOST, PORT)
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
