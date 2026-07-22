# AgentTier General Coding Sandbox Image
# Full development environment with Node.js, Python, Go, and common tools.
# Builds for linux/amd64 and linux/arm64 via buildx TARGETARCH.
#
# Build context: repository root (so the Dockerfile can COPY in the Go
# sources for the bundled `agenttier-sandbox-runtime` binary). The
# release.yml matrix sets context: . and dockerfile:
# images/general-coding/Dockerfile for this reason.

# ---- Stage 1: build the in-pod runtime binary ------------------------------
#
# Bakes a small HTTP server into the sandbox so the Router can proxy
# requests directly instead of going through SPDY exec. See
# pkg/sandboxruntime for design rationale.
# Digest-pinned for supply-chain security (M5).
FROM public.ecr.aws/docker/library/golang:1.25.11-alpine@sha256:523c3effe300580ed375e43f43b1c9b091b68e935a7c3a92bfcc4e7ed55b18c2 AS runtime-build

RUN apk add --no-cache git ca-certificates

ARG TARGETARCH
# GOPROXY=direct bypasses proxy.golang.org on networks where it's blocked
# (corporate VPN / captive networks). Override at build time with
# --build-arg GOPROXY=https://proxy.golang.org,direct on a normal network.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
# GOTOOLCHAIN=local: never auto-download a toolchain; build with the image's
# Go (matches go.mod toolchain go1.25.11) so builds stay hermetic/offline (M5).
ENV GOTOOLCHAIN=local

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

# Copy only the packages the runtime binary actually depends on. This keeps
# the build cache small and avoids invalidating it on changes to unrelated
# parts of the repo (controller, router, web UI, etc.).
COPY cmd/sandbox-runtime ./cmd/sandbox-runtime
COPY pkg/sandboxruntime ./pkg/sandboxruntime
COPY pkg/version ./pkg/version

# CGO disabled for a fully static binary — no glibc dependency, runs on
# any base image including Alpine and distroless variants we may
# downstream from.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' \
    -o /out/agenttier-sandbox-runtime ./cmd/sandbox-runtime

# ---- Stage 2: the actual sandbox image — digest-pinned for supply-chain security (M5).
FROM public.ecr.aws/docker/library/ubuntu:22.04@sha256:4f838adc7181d9039ac795a7d0aba05a9bd9ecd480d294483169c5def983b64d

# Declared by buildx when --platform is set. We re-declare as an ARG so the
# value is visible inside the layer that downloads arch-specific tarballs.
ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive

# System packages
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    git \
    curl \
    wget \
    ca-certificates \
    openssh-client \
    build-essential \
    jq \
    unzip \
    vim \
    less \
    htop \
    tree \
    ripgrep \
    fd-find \
    tmux \
    && rm -rf /var/lib/apt/lists/*

# Node.js 20 LTS (nodesource setup handles both architectures).
# Deliberately NO `npm install -g npm@latest`: the image ships the npm that
# the pinned Node major bundles. An unpinned "latest" broke this build on
# 2026-07-16 when npm 12 released requiring Node >= 22 (EBADENGINE) — the
# same class of drift digest-pinning exists to prevent.
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*

# Python 3.11
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3.11 \
    python3.11-venv \
    python3-pip \
    && ln -sf /usr/bin/python3.11 /usr/bin/python3 \
    && ln -sf /usr/bin/python3.11 /usr/bin/python \
    && rm -rf /var/lib/apt/lists/*

# Go 1.25 — install the right tarball for the build architecture.
RUN case "${TARGETARCH}" in \
      amd64) GOARCH=amd64 ;; \
      arm64) GOARCH=arm64 ;; \
      *) echo "unsupported arch: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL "https://go.dev/dl/go1.25.11.linux-${GOARCH}.tar.gz" | tar -C /usr/local -xzf -
ENV PATH="/usr/local/go/bin:${PATH}"

# In-pod runtime binary + entrypoint that backgrounds it.
COPY --from=runtime-build /out/agenttier-sandbox-runtime /usr/local/bin/agenttier-sandbox-runtime
COPY images/general-coding/entrypoint.sh /usr/local/bin/agenttier-entrypoint
RUN chmod 0755 /usr/local/bin/agenttier-sandbox-runtime /usr/local/bin/agenttier-entrypoint

# Create non-root user
RUN useradd -m -u 1000 -s /bin/bash sandbox \
    && mkdir -p /workspace \
    && chown sandbox:sandbox /workspace

USER sandbox
WORKDIR /workspace

# Configure git
RUN git config --global init.defaultBranch main

# Background the runtime, then run the user's command. CMD stays
# `sleep infinity` for compatibility with the existing template default —
# the entrypoint script simply exec's whatever CMD becomes.
ENTRYPOINT ["/usr/local/bin/agenttier-entrypoint"]
CMD ["sleep", "infinity"]
