// RelayForge Phase 6 k6 Load Script (S1 Ingest / S2 Steady-State / S3 Burst / S9 Read Load)
// Usage:
//   docker run --rm -i --network relayforge_default \
//     -e BASE_URL=http://api:8080 -e API_KEY="$RELAYFORGE_API_KEY" -e SCENARIO=s2 \
//     grafana/k6:0.54.0 run - < bench/k6_ingest.js

import http from 'k6/http';
import { check } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://127.0.0.1:8080';
const API_KEY = __ENV.API_KEY || '';
const SCENARIO = (__ENV.SCENARIO || 's2').toLowerCase();

const scenarios = {
  s1: {
    executor: 'constant-arrival-rate',
    rate: 10,
    timeUnit: '1s',
    duration: __ENV.DURATION || '2m',
    preAllocatedVUs: 16,
    maxVUs: 64,
    exec: 'ingestEvent',
  },
  s2: {
    executor: 'constant-arrival-rate',
    rate: 10,
    timeUnit: '1s',
    duration: __ENV.DURATION || '10m',
    preAllocatedVUs: 16,
    maxVUs: 64,
    exec: 'ingestEvent',
  },
  s3: {
    executor: 'ramping-arrival-rate',
    startRate: 50,
    timeUnit: '1s',
    preAllocatedVUs: 32,
    maxVUs: 128,
    stages: [
      { target: 50, duration: '1m' },
      { target: 10, duration: '5m' },
    ],
    exec: 'ingestEvent',
  },
  s9: {
    executor: 'constant-arrival-rate',
    rate: 50,
    timeUnit: '1s',
    duration: __ENV.DURATION || '2m',
    preAllocatedVUs: 24,
    maxVUs: 64,
    exec: 'readPagination',
  },
};

export const options = {
  scenarios: {
    selected: scenarios[SCENARIO] || scenarios.s2,
  },
  thresholds: {
    http_req_failed: ['rate<0.001'],
    http_req_duration: ['p(95)<150', 'p(99)<400'],
  },
};

const EVENT_TYPES = [
  'order.created',
  'payment.succeeded',
  'invoice.paid',
  'shipment.dispatched',
];

const PADDING_2K = 'x'.repeat(1850);

export function ingestEvent() {
  const iter = `${__VU}-${__ITER}-${Date.now()}`;
  const evType = EVENT_TYPES[(__VU + __ITER) % EVENT_TYPES.length];
  const body = JSON.stringify({
    type: evType,
    occurred_at: new Date().toISOString(),
    data: {
      sequence: iter,
      amount_cents: 14999,
      currency: 'USD',
      blob: PADDING_2K,
    },
  });

  const res = http.post(`${BASE_URL}/api/v1/events`, body, {
    headers: {
      Authorization: `Bearer ${API_KEY}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': `k6-${SCENARIO}-${iter}`,
    },
  });

  check(res, {
    'status is 202': (r) => r.status === 202,
  });
}

export function readPagination() {
  const path =
    __ITER % 2 === 0
      ? '/api/v1/events?type=order.created&limit=25'
      : '/api/v1/deliveries?status=dead_lettered&limit=25';
  const res = http.get(`${BASE_URL}${path}`, {
    headers: {
      Authorization: `Bearer ${API_KEY}`,
    },
  });
  check(res, {
    'status is 200': (r) => r.status === 200,
  });
}
