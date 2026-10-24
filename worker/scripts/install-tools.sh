#!/usr/bin/env bash
set -euo pipefail

export PATH=/usr/local/cargo/bin:${PATH}
rustup component add rustfmt clippy
rustup component list --installed
cargo fmt --version
cargo clippy --version
