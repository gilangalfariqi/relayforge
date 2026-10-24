#!/usr/bin/env bash
# Local/CI helper. The official rust image does not put cargo on PATH for
# non-login shells.
set -euo pipefail

export PATH=/usr/local/cargo/bin:${PATH}

cargo fmt --check
cargo clippy --all-targets --locked -- -D warnings
cargo test --all --locked
