#!/usr/bin/env node
/** Exercise the current next-on-pages artifact with fixture secrets and a real D1 binding. */
import nodeAssert from "node:assert/strict";
import {
  readFile,
  access,
  writeFile,
  mkdir,
  readdir,
  stat,
} from "node:fs/promises";
import { dirname, join, resolve, sep } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { gzipSync } from "node:zlib";

const assertionCounts = {};
let assertionSection = "artifactFreshness";
const recordAssertion = () => {
  assertionCounts[assertionSection] =
    (assertionCounts[assertionSection] || 0) + 1;
};
const assert = new Proxy(nodeAssert, {
  apply(target, thisArg, args) {
    recordAssertion();
    return Reflect.apply(target, thisArg, args);
  },
  get(target, property, receiver) {
    const value = Reflect.get(target, property, receiver);
    return typeof value === "function"
      ? (...args) => {
          recordAssertion();
          return Reflect.apply(value, target, args);
        }
      : value;
  },
});

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const serverDir = resolve(process.argv[2] || join(root, "UptimeFlare"));
const staticDir = join(serverDir, ".vercel/output/static");
const artifact = join(staticDir, "_worker.js/index.js");
const { Miniflare } = await import(
  pathToFileURL(
    join(serverDir, "worker/node_modules/miniflare/dist/src/index.js"),
  )
);
const tokens = {
  p1: "pages-smoke-first-fixture-token-123456",
  p2: "pages-smoke-second-fixture-token-123456",
};
const password = "pages-smoke-admin-password-123456789";
await access(artifact);
async function files(directory, accepts) {
  const entries = await readdir(directory, { withFileTypes: true });
  const nested = await Promise.all(
    entries.map((entry) =>
      entry.isDirectory()
        ? files(join(directory, entry.name), accepts)
        : accepts(entry.name)
        ? [join(directory, entry.name)]
        : [],
    ),
  );
  return nested.flat();
}
// A stale deployment bundle can make a smoke test pass while omitting the new source behavior.
const sourceFiles = [
  join(serverDir, "uptime.config.ts"),
  join(serverDir, "middleware.ts"),
  ...(
    await Promise.all(
      ["components", "pages", "types", "util", "worker/src", "locales"].map(
        (directory) =>
          files(
            join(serverDir, directory),
            (name) =>
              /\.(tsx?|css|json)$/.test(name) && !name.includes(".test."),
          ),
      ),
    )
  ).flat(),
];
const newestSource = Math.max(
  ...(await Promise.all(
    sourceFiles.map(async (path) => (await stat(path)).mtimeMs),
  )),
);
assert(
  (await stat(artifact)).mtimeMs >= newestSource,
  "Pages artifact predates a source change; rebuild before running this test",
);
const modules = (
  await files(dirname(artifact), (name) => name.endsWith(".js"))
).map((path) => ({ type: "ESModule", path }));
const mf = new Miniflare({
  cf: { country: "SG", city: "Singapore", asn: 64512 },
  modules: [
    { type: "ESModule", path: artifact },
    ...modules.filter((module) => module.path !== artifact),
  ],
  modulesRoot: dirname(artifact),
  compatibilityDate: "2025-04-02",
  compatibilityFlags: ["nodejs_compat"],
  d1Databases: ["UPTIMEFLARE_D1"],
  bindings: {
    PROBE_TOKENS: JSON.stringify(tokens),
    ADMIN_PASSWORD: password,
    ADMIN_SESSION_SECRET: "pages-smoke-session-secret-at-least-32-characters",
  },
  serviceBindings: {
    ASSETS: async (request) => {
      const pathname = decodeURIComponent(new URL(request.url).pathname);
      if (pathname.startsWith("/_worker.js"))
        return new Response(null, { status: 404 });
      const path = resolve(staticDir, "." + pathname);
      if (path !== staticDir && !path.startsWith(staticDir + sep))
        return new Response(null, { status: 404 });
      for (const candidate of [
        path,
        path + ".html",
        join(path, "index.html"),
      ]) {
        try {
          if (!(await stat(candidate)).isFile()) continue;
          const contentType = candidate.endsWith(".html")
            ? "text/html; charset=utf-8"
            : candidate.endsWith(".js")
            ? "application/javascript"
            : candidate.endsWith(".css")
            ? "text/css"
            : "application/octet-stream";
          return new Response(await readFile(candidate), {
            headers: { "Content-Type": contentType },
          });
        } catch {
          /* Match Pages' static fallback when an asset does not exist. */
        }
      }
      return new Response(null, { status: 404 });
    },
  },
});
const outcomes = [];
const probeHeaders = (id) => ({ Authorization: `Bearer ${tokens[id]}` });
let cookie = "";
const adminHeaders = () => ({
  "Content-Type": "application/json",
  Origin: "https://pages.test",
  Cookie: cookie,
});
async function check(path, init, status, assertion) {
  const response = await mf.dispatchFetch(`https://pages.test${path}`, init);
  const text = await response.text();
  assert.equal(response.status, status, `${path}: ${text.slice(0, 1000)}`);
  if (assertion) assertion(JSON.parse(text));
  outcomes.push({ path, method: init?.method || "GET", status });
  return response;
}
async function html(path) {
  const response = await mf.dispatchFetch(`https://pages.test${path}`);
  const text = await response.text();
  assert.equal(response.status, 200, `${path}: ${text.slice(0, 500)}`);
  assert(response.headers.get("Content-Type")?.includes("text/html"));
  outcomes.push({ path, method: "GET", status: 200 });
  return text;
}
function pageData(html) {
  const match =
    /<script\b[^>]*\bid="__NEXT_DATA__"[^>]*>([\s\S]*?)<\/script>/.exec(html);
  assert(match, "Expected serialized Next page props");
  return JSON.parse(match[1]).props.pageProps;
}
function noSecrets(value) {
  const encoded = typeof value === "string" ? value : JSON.stringify(value);
  for (const secret of [
    "private-",
    ...Object.values(tokens),
    password,
    "pages-smoke-session-secret",
  ])
    assert(
      !encoded.includes(secret),
      `Public output contains fixture credential marker: ${secret}`,
    );
}
async function save(config) {
  let result;
  await check(
    "/api/admin/config",
    { method: "PUT", headers: adminHeaders(), body: JSON.stringify(config) },
    200,
    (value) => {
      result = value;
    },
  );
  return result;
}
async function ingest(probeId, batchId, results, status = 200) {
  return check(
    "/api/probes/ingest",
    {
      method: "POST",
      headers: {
        ...probeHeaders(probeId),
        "Content-Type": "application/json",
        "Content-Encoding": "gzip",
      },
      body: gzipSync(
        JSON.stringify({ version: 1, batch_id: batchId.repeat(64), results }),
      ),
    },
    status,
    (value) => {
      if (status === 200) assert.equal(value.accepted, results.length);
    },
  );
}
try {
  assertionSection = "authenticationAndLegacyCompatibility";
  const db = await mf.getD1Database("UPTIMEFLARE_D1");
  const schema = await readFile(join(serverDir, "init.sql"), "utf8");
  for (const sql of schema.split(";").filter((sql) => sql.trim()))
    await db.prepare(sql).run();
  await check(
    "/api/probes/config",
    { headers: probeHeaders("p1") },
    200,
    (value) => {
      assert.equal(value.version, 1);
      assert.equal(value.probe_id, "p1");
      assert.deepEqual(value.monitors, []);
      assert(!JSON.stringify(value).includes(tokens.p1));
    },
  );
  await check("/api/probes/config", {}, 401);
  await ingest(
    "p1",
    "0",
    [
      {
        monitor_id: "unassigned-smoke",
        time: Math.floor(Date.now() / 1000),
        up: true,
        latency_ms: 1,
      },
    ],
    403,
  );
  await check("/api/probes/ingest", { headers: probeHeaders("p1") }, 405);
  await check("/api/data", {}, 200, (value) => assert(value.unknown >= 1));
  await check("/api/admin/config", {}, 401);
  await check(
    "/api/admin/login",
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Origin: "https://evil.test",
      },
      body: JSON.stringify({ password }),
    },
    403,
  );
  const login = await check(
    "/api/admin/login",
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Origin: "https://pages.test",
      },
      body: JSON.stringify({ password }),
    },
    200,
  );
  assert(
    login.headers
      .get("Set-Cookie")
      .includes("Secure; HttpOnly; SameSite=Strict"),
  );
  cookie = login.headers.get("Set-Cookie").split(";")[0];
  let initial;
  await check(
    "/api/admin/config",
    { headers: { Cookie: cookie } },
    200,
    (value) => {
      initial = value;
      assert(value.probes.some((probe) => probe.id === "cloudflare"));
      assert(!("probeStaleAfterSeconds" in value));
    },
  );
  const now = Math.floor(Date.now() / 1000);
  assertionSection = "dynamicSettingsAndAssignedConfiguration";
  const config = {
    revision: initial.revision,
    probes: [
      { id: "p1" },
      { id: "p2", name: "Second fixture" },
      { id: "cloudflare" },
    ],
    notificationTemplates: [
      {
        id: "smoke-hook",
        name: "Smoke webhook",
        type: "webhook",
        webhook: {
          url: "https://private-notification.example/secret",
          payloadType: "json",
          headers: { Authorization: "private-notification-secret" },
          payload: { text: "$MSG", credential: "private-notification-body" },
        },
      },
    ],
    notification: {
      timeZone: "Asia/Singapore",
      gracePeriod: 5,
      skipNotificationIds: ["smoke-other"],
      skipErrorChangeNotification: true,
    },
    monitors: [
      {
        id: "smoke",
        name: "Fixture web",
        method: "GET",
        target: "https://private-smoke.example",
        probes: ["p1", "p2", "cloudflare"],
        headers: { Authorization: "private-target-secret" },
        responseKeyword: "private-required-keyword",
        notificationTemplateId: "smoke-hook",
        notificationGracePeriodSeconds: 30,
      },
      {
        id: "smoke-cert",
        name: "Fixture certificate",
        method: "SSL_CERT",
        target: "https://private-cert.example",
        probes: ["p1", "cloudflare"],
        certificateExpiryDays: 21,
        intervalSeconds: 60,
        timeout: 7000,
        checkProxy: "https://private-proxy.example/v1/check",
        checkProxyHeaders: { Authorization: "Bearer private-proxy-secret" },
      },
      {
        id: "smoke-ping",
        name: "Fixture ping",
        method: "ICMP_PING",
        target: "private-ping.example",
        probes: ["p1", "cloudflare"],
        icmpProxyURL: "https://private-ping-proxy.example/v1/ping",
        headers: { Authorization: "Bearer private-icmp-secret" },
      },
      {
        id: "smoke-other",
        name: "Fixture second site",
        method: "HEAD",
        target: "https://private-other.example",
        probes: ["p2"],
        headers: { Authorization: "private-other-secret" },
      },
    ],
    page: {
      title: "Fixture distributed status",
      logo: "/fixture-logo.svg",
      favicon: "/fixture-icon.png",
      links: [
        {
          label: "Fixture docs",
          link: "https://docs.example.com",
          highlight: true,
        },
      ],
      group: {
        "Fixture web group": ["smoke", "smoke-cert"],
        "Fixture network group": ["smoke-ping", "smoke-other"],
      },
      maintenances: { upcomingColor: "teal" },
      customFooter:
        '<p>Fixture custom footer</p><script>window.__unsafeFooter=true</script><a href="javascript:alert(1)">Invalid link</a>',
    },
    maintenances: [
      {
        id: "smoke-maint",
        title: "Fixture current maintenance",
        body: "Fixture current maintenance description",
        start: new Date((now - 60) * 1000).toISOString(),
        end: new Date((now + 1800) * 1000).toISOString(),
        monitors: ["smoke"],
      },
      {
        id: "smoke-upcoming",
        title: "Fixture future maintenance",
        body: "Fixture future maintenance description",
        start: new Date((now + 3600) * 1000).toISOString(),
        end: new Date((now + 7200) * 1000).toISOString(),
        monitors: ["smoke-cert"],
        repeat: { frequency: "daily", timeZone: "Asia/Singapore" },
      },
    ],
  };
  let saved = await save(config);
  assert.equal(saved.monitors[1].method, "SSL_CERT");
  assert.equal(saved.monitors[1].certificateExpiryDays, 21);
  assert.equal(
    saved.monitors[1].checkProxyHeaders.Authorization,
    "Bearer private-proxy-secret",
  );
  assert.equal(saved.monitors[2].method, "ICMP_PING");
  assert.equal(saved.monitors[2].icmpProxyURL, config.monitors[2].icmpProxyURL);
  assert.equal(saved.monitors[0].notificationGracePeriodSeconds, 30);
  assert.equal(saved.notification.gracePeriod, 5);
  assert.deepEqual(saved.page.group, config.page.group);
  assert.equal(saved.maintenances[1].repeat.frequency, "daily");
  assert(!saved.page.customFooter.includes("__unsafeFooter"));
  assert(!saved.page.customFooter.includes("javascript:"));
  await check(
    "/api/admin/config",
    { method: "PUT", headers: adminHeaders(), body: JSON.stringify(config) },
    409,
  );
  await check(
    "/api/probes/config",
    { headers: probeHeaders("p1") },
    200,
    (value) => {
      assert.deepEqual(
        value.monitors.map((monitor) => monitor.id),
        ["smoke", "smoke-cert", "smoke-ping"],
      );
      assert.equal(value.monitors[0].intervalSeconds, 300);
      assert.equal(value.monitors[0].timeout, 5000);
      assert.equal(
        value.monitors[0].headers.Authorization,
        "private-target-secret",
      );
      assert.equal(value.monitors[1].certificateExpiryDays, 21);
      assert.equal(value.monitors[1].intervalSeconds, 60);
      assert.equal(value.monitors[1].timeout, 7000);
      assert.equal(
        value.monitors[1].checkProxyHeaders.Authorization,
        "Bearer private-proxy-secret",
      );
      assert.equal(
        value.monitors[2].icmpProxyURL,
        config.monitors[2].icmpProxyURL,
      );
      const encoded = JSON.stringify(value);
      assert(!encoded.includes("private-other"));
      assert(!encoded.includes("private-notification"));
      assert(!encoded.includes("notificationTemplateId"));
      assert(!encoded.includes("notificationGracePeriodSeconds"));
      assert(!encoded.includes("Fixture current maintenance"));
    },
  );
  await check(
    "/api/probes/config",
    { headers: probeHeaders("p2") },
    200,
    (value) => {
      assert.deepEqual(
        value.monitors.map((monitor) => monitor.id),
        ["smoke", "smoke-other"],
      );
      assert.equal(
        value.monitors[1].headers.Authorization,
        "private-other-secret",
      );
      assert(!JSON.stringify(value).includes("private-proxy"));
      assert(!JSON.stringify(value).includes("private-icmp"));
      assert(!JSON.stringify(value).includes("private-notification"));
    },
  );
  assertionSection = "gzipIngestionAndMetadata";
  assert.equal(
    (await db.prepare("SELECT COUNT(*) n FROM probe_samples").first()).n,
    0,
  );
  const successBucket = Math.floor(now / 300) * 300 - 600;
  const oldTime = now - 85 * 86400;
  const p1Results = [
    {
      monitor_id: "smoke",
      time: oldTime,
      up: false,
      latency_ms: 2,
      stage: "dns",
      code: "not_found",
      message: "DNS name was not found",
    },
    { monitor_id: "smoke", time: now - 13 * 3600, up: true, latency_ms: 17 },
    ...["tcp", "http", "body"].map((stage, index) => ({
      monitor_id: "smoke",
      time: now - 1500 - index * 300,
      up: false,
      latency_ms: 30 + index,
      stage,
      code: ["refused", "status", "keyword"][index],
      message: "Fixture safe failure",
    })),
    { monitor_id: "smoke", time: successBucket + 10, up: true, latency_ms: 11 },
    { monitor_id: "smoke", time: successBucket + 20, up: true, latency_ms: 13 },
    { monitor_id: "smoke", time: now - 5, up: true, latency_ms: 9 },
    {
      monitor_id: "smoke-cert",
      time: now - 4,
      up: false,
      latency_ms: 8,
      stage: "tls",
      code: "expiring",
      message: "TLS certificate expires within the configured threshold",
      certificate_expires_at: now + 3 * 86400,
      certificate_days_remaining: 3,
    },
    {
      monitor_id: "smoke-ping",
      time: now - 3,
      up: true,
      latency_ms: 5,
      icmp_latency_ms: 2.75,
    },
  ];
  const p2Results = [
    {
      monitor_id: "smoke",
      time: now - 4,
      up: false,
      latency_ms: 4,
      stage: "tcp",
      code: "refused",
      message: "TCP connection was refused",
    },
    { monitor_id: "smoke-other", time: now - 3, up: true, latency_ms: 7 },
  ];
  await ingest("p1", "1", p1Results);
  await ingest("p2", "2", p2Results);
  await ingest("p1", "1", p1Results);
  await ingest(
    "p2",
    "3",
    [{ monitor_id: "smoke-cert", time: now - 2, up: true, latency_ms: 1 }],
    403,
  );
  await ingest("p1", "4", [
    {
      monitor_id: "smoke-cert",
      time: now - 60,
      up: true,
      latency_ms: 3,
      certificate_expires_at: now + 20 * 86400,
      certificate_days_remaining: 20,
    },
  ]);
  const expectedSamples = p1Results.length + p2Results.length + 1;
  assert.equal(
    (await db.prepare("SELECT COUNT(*) n FROM probe_samples").first()).n,
    expectedSamples,
  );
  assertionSection = "publicSummariesAndBoundedHistory";
  await check("/api/data", {}, 200, (value) => {
    noSecrets(value);
    const web = value.monitors.smoke;
    assert.equal(web.status, "degraded");
    assert.equal(web.up, false);
    assert.equal(web.reachableProbes, 1);
    assert.equal(web.unreachableProbes, 1);
    assert.equal(web.unknownProbes, 1);
    assert.equal(web.total, 3);
    const probe = web.probes.find((probe) => probe.id === "p1");
    assert.equal(probe.name, "SG / Singapore · AS64512");
    assert.equal(probe.checks, 8);
    assert.equal(probe.failures, 4);
    assert(
      probe.history.every((bucket) => bucket.time >= now - 12 * 3600 - 300),
    );
    assert(probe.history.length <= 145);
    const bucket = probe.history.find(
      (bucket) => bucket.time === successBucket,
    );
    assert.equal(bucket.checks, 2);
    assert.equal(bucket.failures, 0);
    assert.equal(bucket.avgLatencyMs, 12);
    assert(
      probe.dailyHistory.some(
        (day) => day.time === Math.floor(oldTime / 86400) * 86400,
      ),
    );
    assert(probe.dailyHistory.length <= 91);
    assert.equal(probe.uptimePercent, 50);
    const cert = value.monitors["smoke-cert"].probes.find(
      (probe) => probe.id === "p1",
    );
    assert.equal(cert.status, "down");
    assert.equal(cert.certificateExpiresAt, now + 3 * 86400);
    assert.equal(cert.certificateDaysRemaining, 3);
    const ping = value.monitors["smoke-ping"].probes.find(
      (probe) => probe.id === "p1",
    );
    assert.equal(ping.icmpLatencyMs, 2.75);
  });
  assertionSection = "publicPageServerProps";
  const home = await html("/");
  noSecrets(home);
  const props = pageData(home);
  // Existing NoSsr deliberately renders the visible app in the browser. Verify the
  // actual server props here; browser QA covers title, groups, charts and labels.
  assert.equal(props.page.title, "Fixture distributed status");
  assert.deepEqual(props.page.group, config.page.group);
  assert.equal(props.page.logo, "/fixture-logo.svg");
  assert.equal(props.page.favicon, "/fixture-icon.png");
  assert.deepEqual(props.page.links, config.page.links);
  assert.equal(props.page.maintenances.upcomingColor, "teal");
  assert(props.page.customFooter.includes("Fixture custom footer"));
  assert(!props.page.customFooter.includes("__unsafeFooter"));
  assert(!props.page.customFooter.includes("javascript:"));
  assert.deepEqual(props.maintenances[0], saved.maintenances[0]);
  assert.equal(props.maintenances[1].repeat.timeZone, "Asia/Singapore");
  assert.equal(
    props.probeSummaries["smoke-cert"].probes.find((probe) => probe.id === "p1")
      .certificateDaysRemaining,
    3,
  );
  for (const monitor of props.monitors) {
    assert(
      !("target" in monitor) &&
        !("headers" in monitor) &&
        !("checkProxy" in monitor),
    );
    assert(!("notificationTemplateId" in monitor));
  }
  assertionSection = "adminSixModules";
  const admin = await html("/admin");
  noSecrets(admin);
  assert.deepEqual(
    pageData(admin),
    {},
    "Admin static shell must not embed configuration or credentials",
  );
  const adminBundles = await files(join(staticDir, "_next"), (name) =>
    /^admin-[\w-]+\.js$/.test(name),
  );
  assert(adminBundles.length, "Expected the actual admin client bundle");
  const adminCode = (
    await Promise.all(adminBundles.map((path) => readFile(path, "utf8")))
  ).join("\n");
  assert(adminCode.includes("配置管理"));
  for (const module of [
    "monitors",
    "probes",
    "notifications",
    "groups",
    "maintenances",
    "page",
  ])
    assert(
      new RegExp(`["']?value["']?\\s*:\\s*["']${module}["']`).test(adminCode),
      `Admin bundle omitted module ${module}`,
    );
  assertionSection = "publicIncidentServerProps";
  const incidents = await html("/incidents?monitor=smoke");
  noSecrets(incidents);
  const incidentProps = pageData(incidents);
  assert.equal(incidentProps.initialMonitor, "smoke");
  assert.equal(incidentProps.page.title, config.page.title);
  assert.deepEqual(incidentProps.page.group, config.page.group);
  assert(
    incidentProps.monitors.some((monitor) => monitor.name === "Fixture web"),
  );
  assert(
    incidentProps.initialHistory.probes.failures.some(
      (failure) => failure.monitorId === "smoke",
    ),
  );
  assertionSection = "incidentPaginationAndMonthlyFilters";
  const failures = [];
  let cursor;
  for (let page = 0; page < 5; page++) {
    let result;
    const query = new URLSearchParams({
      kind: "probes",
      monitor: "smoke",
      from: String(now - 90 * 86400),
      to: String(now + 1),
      limit: "2",
      ...(cursor && { cursor }),
    });
    await check(`/api/incidents?${query}`, {}, 200, (value) => {
      noSecrets(value);
      result = value.probes;
      assert.equal(value.native, null);
    });
    assert(result.failures.length <= 2);
    failures.push(...result.failures);
    cursor = result.nextCursor;
    if (!cursor) break;
  }
  assert.equal(cursor, null);
  assert.equal(failures.length, 5);
  assert.equal(
    new Set(
      failures.map(
        (failure) => `${failure.time}/${failure.monitorId}/${failure.probeId}`,
      ),
    ).size,
    5,
  );
  assert(failures.some((failure) => failure.time === oldTime));
  await check("/api/incidents?kind=native&monitor=smoke", {}, 200, (value) =>
    assert.deepEqual(value.native.incidents, []),
  );
  await check("/api/incidents?kind=probes&limit=101", {}, 400);
  await check("/api/incidents?kind=probes&cursor=invalid", {}, 400);
  const oldDate = new Date(oldTime * 1000);
  const monthFrom =
    Date.UTC(oldDate.getUTCFullYear(), oldDate.getUTCMonth(), 1) / 1000;
  const monthTo =
    Date.UTC(oldDate.getUTCFullYear(), oldDate.getUTCMonth() + 1, 1) / 1000;
  await check(
    `/api/incidents?kind=probes&monitor=smoke&probe=p1&from=${monthFrom}&to=${monthTo}`,
    {},
    200,
    (value) =>
      assert.deepEqual(
        value.probes.failures.map((failure) => failure.time),
        [oldTime],
      ),
  );
  assertionSection = "remoteIntervalAndTimeout";
  saved.monitors[0].intervalSeconds = 60;
  saved.monitors[0].timeout = 7000;
  saved = await save(saved);
  await check(
    "/api/probes/config",
    { headers: probeHeaders("p1") },
    200,
    (value) => {
      assert.equal(value.monitors[0].intervalSeconds, 60);
      assert.equal(value.monitors[0].timeout, 7000);
      assert.equal(value.monitors[2].intervalSeconds, 300);
      assert.equal(value.monitors[2].timeout, 5000);
    },
  );
  assertionSection = "automaticIdentityConflictResolution";
  const duplicate = await save({
    ...saved,
    monitors: [...saved.monitors, saved.monitors[0]],
    notificationTemplates: [
      ...saved.notificationTemplates,
      { ...saved.notificationTemplates[0], id: undefined },
    ],
  });
  assert.equal(duplicate.monitors[0].id, "smoke");
  assert.notEqual(duplicate.monitors.at(-1).id, "smoke");
  assert.notEqual(
    duplicate.notificationTemplates[0].id,
    duplicate.notificationTemplates[1].id,
  );
  const report = {
    passed: true,
    actualPagesArtifact: artifact,
    artifactSourceFreshness: true,
    processEnvSecretBridge: true,
    middlewareAndApiRoutes: true,
    realD1Binding: true,
    authenticatedAdminAndDynamicConfiguration: true,
    adminSixModulesInClientBundle: true,
    publicPageServerPropsGroupsMaintenance: true,
    visiblePageRendering:
      "client via existing NoSsr; verified separately in browser QA",
    footerSanitization: true,
    SSLAndICMPConfigurationRoundtrip: true,
    perProbeAssignmentAndProxyHeaderIsolation: true,
    publicSecretIsolation: true,
    gzipSamplesAndCertificateICMPMetadata: true,
    monotonicMetadataAndIdempotency: true,
    twelveHourLatencyAndNinetyDayHistory: true,
    paginatedAndMonthlyIncidentHistory: true,
    automaticIdentityConflictResolution: true,
    assertionCounts,
    totalAssertions: Object.values(assertionCounts).reduce(
      (sum, count) => sum + count,
      0,
    ),
    newFeatureAssertions: Object.entries(assertionCounts)
      .filter(
        ([section]) =>
          ![
            "authenticationAndLegacyCompatibility",
            "automaticIdentityConflictResolution",
            "remoteIntervalAndTimeout",
          ].includes(section),
      )
      .reduce((sum, [, count]) => sum + count, 0),
    sampleCount: expectedSamples,
    checks: outcomes,
  };
  await mkdir(join(root, "bin"), { recursive: true });
  await writeFile(
    join(root, "bin/pages-smoke-report.json"),
    JSON.stringify(report, null, 2) + "\n",
  );
  console.log(JSON.stringify(report, null, 2));
} finally {
  await mf.dispose();
}
