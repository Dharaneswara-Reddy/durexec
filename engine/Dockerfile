# Two stages. The first has a Go toolchain, a package cache and a shell; the
# second has the binary and nothing else — no shell, no package manager, no
# libc to link against. Anything an attacker would need to pivot with simply
# is not in the image.

FROM golang:1.25 AS builder

WORKDIR /src

# Dependencies are copied first so a source-only change reuses the module
# download layer instead of refetching the whole graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 is what makes the binary static, which is what lets it run on
# an image with no C library at all. -trimpath keeps build machine paths out of
# the binary; -s -w drops the symbol and DWARF tables, which are of no use in
# production and are a meaningful share of the image size.
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV CGO_ENABLED=0

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/engined ./cmd/engined

# The container healthcheck needs to make an HTTP request, and the runtime
# image has no curl and no shell to run one with. So the probe is a second
# static binary, a few kilobytes, built from deploy/healthcheck.
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
        -trimpath -ldflags="-s -w" \
        -o /out/healthcheck ./deploy/healthcheck

# ---------------------------------------------------------------------------

# distroless/static carries CA certificates, /etc/passwd and timezone data and
# nothing else. The :nonroot tag runs as uid 65532, so the process cannot write
# anywhere it was not explicitly given.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /

COPY --from=builder /out/engined /engined
COPY --from=builder /out/healthcheck /healthcheck

# The schema ships with the binary. engined applies migrations at startup, so a
# node that cannot find them cannot boot — and a distroless image has no shell
# to go fetch them with. MIGRATIONS_DIR overrides the location if needed.
COPY --from=builder /src/migrations /migrations

# Stated again rather than inherited, so a change to the base tag cannot
# silently promote this container to root.
USER 65532:65532

EXPOSE 8080

# Exec form: the healthcheck binary exists specifically so this can run
# without a shell. Interval/timeout/retries are tuned to the compose demo
# cluster's LEASE_TTL (10s) — long enough that one slow request during a
# failover does not flap the container, short enough that a genuinely wedged
# node is reported before an operator would notice by hand.
HEALTHCHECK --interval=5s --timeout=3s --start-period=10s --retries=5 \
    CMD ["/healthcheck"]

# There is no shell in this image, so the entrypoint is exec form. A string
# form would try to run /bin/sh -c and fail.
ENTRYPOINT ["/engined"]
