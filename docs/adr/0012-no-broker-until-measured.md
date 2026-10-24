# ADR-0012: No message broker until a measured threshold

Status: Accepted (implemented; last reviewed 2026-10-24). Date: 2026-10-22.

## Context

Kafka/Redis/SQS would decouple workers from PostgreSQL lock contention at high throughput. They also
reintroduce a dual-write problem unless an outbox is designed first.

## Decision

v1 uses only PostgreSQL. Adding a broker is a new ADR that must include:

1. The measured bottleneck (claim latency, queue age, or throughput) from Phase 6 methodology.
2. An outbox or equivalent so event insert and enqueue remain atomic.
3. Operational cost (another cluster, another failure domain).

Until then, scale path is: tune autovacuum, partition `deliveries` by time, hash-bucket claim
queries, add workers.

## Alternatives considered

Rejected as a v1 default: Kafka, NATS, Redis Streams, SQS, RabbitMQ.
