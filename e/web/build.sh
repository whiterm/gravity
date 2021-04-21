#!/usr/bin/env sh
set -eux

: ${BUILDKIT_HOST=unix:///run/buildkitd/buildkitd.sock}

# --output type=image,name=gravity-web:1.0.0,push=true
buildctl --addr=${BUILDKIT_HOST} build \
    --frontend=dockerfile.v0 \
    --local context=. \
    --local dockerfile=. \
    --output type=docker,name=gravity-web:1.0.0 | docker load
