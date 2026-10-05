const { test } = require("node:test");
const assert = require("node:assert/strict");
const history = require("../internal/agent/web/history.js");
const now = 1791245100;

test("12h preserves 144 time slots and failure/missing latency gaps", () => {
  const buckets = history.window(
    {
      buckets: [
        {
          time: now - 900,
          checks: 2,
          failures: 0,
          latency_sum: 30,
          latency_checks: 2,
        },
        {
          time: now - 600,
          checks: 2,
          failures: 1,
          latency_sum: 10,
          latency_checks: 1,
        },
        {
          time: now,
          checks: 1,
          failures: 1,
          latency_sum: 0,
          latency_checks: 0,
        },
      ],
    },
    now,
    "12h",
  );
  assert.equal(buckets.length, 144);
  assert.equal(buckets[0].time, now - 143 * 300);
  const points = history.series(buckets, "12h").slice(-4);
  assert.deepEqual(
    points.map((p) => p.value),
    [15, null, null, null],
  );
  assert.deepEqual(
    points.map((p) => p.status),
    ["up", "mixed", "unknown", "down"],
  );
  const desktop = history.segments(buckets, false),
    mobile = history.segments(buckets, true);
  assert.equal(desktop.length, 144);
  assert.equal(
    mobile.reduce((n, s) => n + s.count, 0),
    144,
  );
  assert.equal(mobile[0].status, "unknown");
  assert.equal(mobile[0].checks, 0);
});

test("90d uses UTC days, observed check ratio and successful latency weights", () => {
  const today = Math.floor(now / 86400) * 86400;
  const buckets = history.window(
    {
      daily_buckets: [
        {
          time: today - 86400,
          checks: 10,
          failures: 2,
          latency_sum: 80,
          latency_checks: 8,
        },
        {
          time: today,
          checks: 20,
          failures: 4,
          latency_sum: 320,
          latency_checks: 16,
        },
      ],
    },
    now,
    "90d",
  );
  assert.equal(buckets.length, 90);
  assert.equal(buckets[0].time, today - 89 * 86400);
  assert.deepEqual(
    history
      .series(buckets, "90d")
      .slice(-3)
      .map((p) => p.value),
    [null, 80, 80],
  );
  const last = history.segments(buckets, true).at(-1);
  assert.equal(last.count, 2);
  assert.equal(last.status, "mixed");
  assert.equal(last.checks, 30);
  assert.equal(last.failures, 6);
  assert.equal(last.average, 400 / 24);
});

test("mobile combines only contiguous equal results and keeps real zero latency", () => {
  const buckets = history.window(
    {
      buckets: [
        {
          time: now - 600,
          checks: 1,
          failures: 0,
          latency_sum: 0,
          latency_checks: 1,
        },
        {
          time: now - 300,
          checks: 3,
          failures: 0,
          latency_sum: 60,
          latency_checks: 3,
        },
        {
          time: now,
          checks: 1,
          failures: 1,
          latency_sum: 0,
          latency_checks: 0,
        },
      ],
    },
    now,
    "12h",
  );
  const segments = history.segments(buckets, true);
  assert.equal(segments.at(-2).count, 2);
  assert.equal(segments.at(-2).average, 15);
  assert.equal(segments.at(-1).status, "down");
  assert.equal(segments.at(-1).average, null);
  assert.equal(history.series(buckets, "12h").at(-3).value, 0);
});
