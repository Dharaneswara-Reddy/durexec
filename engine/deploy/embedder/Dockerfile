# The stack's model server: a small stdlib HTTP service in front of
# sentence-transformers' all-MiniLM-L6-v2, used by internal/idem/semantic
# so the engine's dedup check measures meaning instead of falling back to
# the built-in FakeEmbedder's text hash.
#
# torch + sentence-transformers are what make this image large; nothing else
# is added on top of them and the standard library.
FROM python:3.11-slim

WORKDIR /app

# --extra-index-url pulls PyTorch's CPU-only wheel instead of the default
# CUDA build. The CUDA build is several times bigger and there is no GPU
# anywhere in this stack (demo cluster or CI), so it would be pure waste.
# --no-cache-dir keeps pip's download cache out of the image layer.
RUN pip install --no-cache-dir \
        --extra-index-url https://download.pytorch.org/whl/cpu \
        torch==2.4.1+cpu \
        sentence-transformers==3.1.1

# Pin the model weights into the image at build time. Without this, the
# first container start on every fresh machine (and every CI run) would
# reach out to huggingface.co and download ~90MB before it could answer a
# single request — slow, and one more thing that can fail with no network.
# HF_HOME fixes where sentence-transformers looks for that cache, both here
# and at runtime, so the copy made at build time is the one actually used.
ENV HF_HOME=/model-cache
RUN python3 -c "from sentence_transformers import SentenceTransformer; SentenceTransformer('all-MiniLM-L6-v2')"

COPY app.py .

ENV PYTHONUNBUFFERED=1
EXPOSE 8000

CMD ["python3", "/app/app.py"]
