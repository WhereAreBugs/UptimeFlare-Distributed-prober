"use strict";

// Same windows and semantics as the public site's summarizeProbeHistory and
// summarizeProbeDailyHistory. Missing observations never become failures.
const ProbeHistory = (() => {
  function window(data, now, range) {
    const daily = range === "90d",
      step = daily ? 86400 : 300;
    const count = daily ? 90 : 144,
      end = Math.floor(now / step) * step;
    const source = new Map(
      ((daily ? data.daily_buckets : data.buckets) || []).map((b) => [
        b.time,
        b,
      ]),
    );
    return Array.from({ length: count }, (_, i) => {
      const time = end - (count - 1 - i) * step;
      const b = source.get(time),
        checks = b?.checks || 0,
        failures = b?.failures || 0;
      const status = !checks
        ? "unknown"
        : !failures
        ? "up"
        : failures === checks
        ? "down"
        : "mixed";
      const latencyChecks = b?.latency_checks ?? (failures ? 0 : checks);
      const average =
        latencyChecks && (daily || !failures)
          ? b.latency_sum / latencyChecks
          : null;
      return {
        time,
        checks,
        failures,
        status,
        average,
        latencyChecks: average === null ? 0 : latencyChecks,
        uptime: checks ? (100 * (checks - failures)) / checks : null,
        step,
      };
    });
  }
  function segments(buckets, compact) {
    const result = [];
    for (const b of buckets) {
      let s = result.at(-1);
      if (!compact || !s || s.status !== b.status || s.end !== b.time) {
        s = {
          status: b.status,
          start: b.time,
          end: b.time,
          count: 0,
          checks: 0,
          failures: 0,
          latencySum: 0,
          latencyChecks: 0,
          average: null,
        };
        result.push(s);
      }
      s.end = b.time + b.step;
      s.count++;
      s.checks += b.checks;
      s.failures += b.failures;
      s.latencySum += (b.average ?? 0) * b.latencyChecks;
      s.latencyChecks += b.latencyChecks;
      s.average = s.latencyChecks ? s.latencySum / s.latencyChecks : null;
    }
    return result;
  }
  function series(buckets, range) {
    return buckets.map((b) => ({
      ...b,
      value: range === "90d" ? b.uptime : b.average,
    }));
  }
  return { window, segments, series };
})();
if (typeof module !== "undefined") module.exports = ProbeHistory;
