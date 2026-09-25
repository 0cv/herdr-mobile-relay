FROM mcr.microsoft.com/playwright:v1.62.1-noble

USER root
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       ca-certificates libnss3-tools openssl python3 \
    && rm -rf /var/lib/apt/lists/*
