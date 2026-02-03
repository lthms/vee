FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates git jq gh golang-go make \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://claude.ai/install.sh | bash

ENV PATH="/root/.local/bin:${PATH}"

WORKDIR /workspace
