# nexus broker image (NEX-457). The dashboard is go:embed'd into the binary,
# so only the prebuilt nexus binary is needed. No secrets baked in: TLS, the
# DB DSN, and operator/admin config arrive at runtime via mounted secrets/env.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY nexus cw /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/nexus"]
