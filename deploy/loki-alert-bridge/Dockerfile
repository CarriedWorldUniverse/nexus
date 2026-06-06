FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

COPY loki-alert-bridge /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/loki-alert-bridge"]
