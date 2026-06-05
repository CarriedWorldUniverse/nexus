FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

COPY dispatch-controller cw /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/dispatch-controller"]
