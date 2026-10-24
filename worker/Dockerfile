# syntax=docker/dockerfile:1.7

FROM rust:1.99.1-bookworm AS build

WORKDIR /src

# Cache dependency compilation separately from application code.
COPY Cargo.toml Cargo.lock ./
RUN mkdir src \
    && printf '' > src/lib.rs \
    && printf 'fn main() {}\n' > src/main.rs \
    && cargo build --release --locked \
    && rm -rf src

COPY src ./src
RUN touch src/lib.rs src/main.rs && cargo build --release --locked

FROM debian:bookworm-slim AS worker

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /src/target/release/relayforge-worker /worker

ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
USER 65532:65532
EXPOSE 9090
ENTRYPOINT ["/worker"]
