#!/usr/bin/env node
/** Real Go scheduling and live central admin settings against a local Worker + D1. */
import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { createServer } from 'node:http'
import { once } from 'node:events'
import { createHash } from 'node:crypto'
import { access, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const serverDir = resolve(process.argv[2] || join(root, 'UptimeFlare'))
const binary = resolve(process.argv[3] || join(root, 'bin/light-prober-smoke'))
const reportPath = join(root, 'bin/remote-config-smoke-report.json')
const { Miniflare } = await import(pathToFileURL(join(serverDir, 'worker/node_modules/miniflare/dist/src/index.js')))
const { build } = await import(pathToFileURL(join(serverDir, 'worker/node_modules/esbuild/lib/main.js')))
const temporary = await mkdtemp(join(tmpdir(), 'light-prober-remote-config-'))
const tokens = {
  p1: 'remote-config-probe-one-fixture-token-123456',
  p2: 'remote-config-probe-two-fixture-token-123456',
}
const password = 'remote-config-fixture-admin-password-123456'
const sessionSecret = 'remote-config-fixture-session-secret-at-least-32-characters'
const secrets = {
  p1: 'remote-config-p1-target-header-secret',
  p2: 'remote-config-p2-target-header-secret',
  notification: 'remote-config-notification-header-secret',
}
const origin = 'https://receiver.test'
const started = Date.now()
const children = new Set()
const processes = new Map()
const targetEvents = []
const configReads = { p1: 0, p2: 0 }
const ingestErrors = []
const responseTimers = new Set()
let mf, db, target, proxy, cookie, saved, report, binarySHA256
let slowResponseAborts = 0
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))
const events = path => targetEvents.filter(event => event.path === path)
const redact = value => Object.values(tokens).concat(Object.values(secrets), password, sessionSecret)
  .reduce((text, secret) => text.replaceAll(secret, '[fixture-redacted]'), String(value))

async function until(condition, description, timeout = 12000) {
  const deadline = Date.now() + timeout
  while (Date.now() < deadline) {
    for (const child of children) {
      if (child.exitCode !== null || child.signalCode !== null) throw new Error(`Probe exited before ${description}`)
    }
    if (await condition()) return
    await sleep(100)
  }
  throw new Error(`Timed out: ${description}`)
}
async function listen(server) {
  server.listen(0, '127.0.0.1')
  await once(server, 'listening')
  return server.address().port
}
async function close(server) {
  if (!server?.listening) return
  server.closeAllConnections()
  await new Promise(resolve => server.close(resolve))
}
function launch(id) {
  const child = spawn(binary, [
    '--server', `http://127.0.0.1:${proxy.address().port}`, '--allow-insecure',
    '--data-dir', join(temporary, id), '--interval', '1s', '--config-interval', '1s', '--flush-interval', '1s',
  ], { cwd: root, env: { ...process.env, LIGHT_PROBER_TOKEN: tokens[id], OTEL_SDK_DISABLED: 'true' }, stdio: ['ignore', 'pipe', 'pipe'] })
  children.add(child)
  const details = { id, pid: child.pid, logs: '' }
  processes.set(child, details)
  for (const stream of [child.stdout, child.stderr]) stream.on('data', chunk => { details.logs = (details.logs + chunk.toString()).slice(-12000) })
  child.on('error', error => { details.logs += error.message })
  return child
}
async function stop(child) {
  if (child.exitCode !== null || child.signalCode !== null) { children.delete(child); return }
  const exited = once(child, 'exit')
  child.kill('SIGTERM')
  let timer
  try {
    await Promise.race([exited, new Promise((_, reject) => { timer = setTimeout(() => { child.kill('SIGKILL'); reject(new Error('Probe shutdown timed out')) }, 10000) })])
  } finally { clearTimeout(timer); children.delete(child) }
}
async function admin(path, method = 'GET', body) {
  const response = await mf.dispatchFetch(`${origin}/api/admin/${path}`, {
    method, headers: { ...(method !== 'GET' && { Origin: origin, 'Content-Type': 'application/json' }), ...(cookie && { Cookie: cookie }) },
    ...(body !== undefined && { body: JSON.stringify(body) }),
  })
  assert.equal(response.status, 200, `Admin ${method} ${path} must succeed`)
  if (path === 'login') cookie = response.headers.get('Set-Cookie').split(';')[0]
  return response.json()
}
async function probeConfig(id) {
  const response = await mf.dispatchFetch(`${origin}/api/probes/config`, { headers: { Authorization: `Bearer ${tokens[id]}` } })
  assert.equal(response.status, 200, `Probe ${id} configuration must succeed`)
  const value = await response.json()
  assert.equal(value.probe_id, id)
  const encoded = JSON.stringify(value)
  for (const secret of Object.values(tokens).concat(password, sessionSecret, secrets.notification)) assert(!encoded.includes(secret), 'Configuration must not expose receiver or notification credentials')
  assert(!encoded.includes('notificationTemplateId'), 'Notification settings are not target check configuration')
  return value
}
async function cache(id) {
  try { return JSON.parse(await readFile(join(temporary, id, 'config.json'), 'utf8')).config }
  catch { return null }
}
async function rows() {
  return (await db.prepare('SELECT probe_id,monitor_id,time,up,latency_ms,stage,code FROM probe_samples ORDER BY time,probe_id,monitor_id').all()).results
}

try {
  await access(binary)
  binarySHA256 = createHash('sha256').update(await readFile(binary)).digest('hex')
  await mkdir(join(root, 'bin'), { recursive: true })
  await rm(reportPath, { force: true })
  target = createServer((request, response) => {
    const path = new URL(request.url, 'http://target.test').pathname
    targetEvents.push({ path, at: Date.now(), fixture: request.headers['x-fixture-secret'], phase: request.headers['x-fixture-phase'] })
    if (path === '/slow-response') {
      const timer = setTimeout(() => { responseTimers.delete(timer); response.writeHead(200); response.end('ready') }, 400)
      responseTimers.add(timer)
      response.on('close', () => { if (!response.writableEnded) slowResponseAborts++; clearTimeout(timer); responseTimers.delete(timer) })
    } else { response.writeHead(200, { 'Content-Type': 'text/plain' }); response.end('ready') }
  })
  const targetPort = await listen(target)
  const targetURL = path => `http://127.0.0.1:${targetPort}${path}`
  const fallback = { monitors: [], probes: [{ id: 'p1', name: 'First fixture probe' }, { id: 'p2', name: 'Second fixture probe' }], notificationTemplates: [] }
  const entrypoint = `import {handleAdminRequest} from ${JSON.stringify(join(serverDir, 'worker/src/admin.ts'))};
import {getRuntimeConfig} from ${JSON.stringify(join(serverDir, 'worker/src/settings.ts'))};
import {handleProbeRequest,getProbeSummaries} from ${JSON.stringify(join(serverDir, 'worker/src/probes.ts'))};
const fallback=${JSON.stringify(fallback)};
export default {async fetch(request,env){const path=new URL(request.url).pathname;
if(path.startsWith('/api/admin/'))return handleAdminRequest(request,env,fallback);
const config=await getRuntimeConfig(env,fallback);
if(path==='/fixture/summaries')return new Response(JSON.stringify(await getProbeSummaries(env,config.monitors,config.probes)));
return handleProbeRequest(request,env,config.monitors)}};`
  const bundled = await build({ stdin: { contents: entrypoint, resolveDir: serverDir }, bundle: true, write: false, format: 'esm', platform: 'browser', target: 'es2022' })
  mf = new Miniflare({ modules: true, script: bundled.outputFiles[0].text, compatibilityDate: '2025-04-02', d1Databases: ['UPTIMEFLARE_D1'], bindings: { PROBE_TOKENS: JSON.stringify(tokens), ADMIN_PASSWORD: password, ADMIN_SESSION_SECRET: sessionSecret } })
  db = await mf.getD1Database('UPTIMEFLARE_D1')
  const schema = await readFile(join(serverDir, 'init.sql'), 'utf8')
  for (const sql of schema.split(';').filter(sql => sql.trim())) await db.prepare(sql).run()

  proxy = createServer(async (request, response) => {
    try {
      const chunks = []
      for await (const chunk of request) chunks.push(chunk)
      const body = Buffer.concat(chunks)
      const id = Object.keys(tokens).find(id => request.headers.authorization === `Bearer ${tokens[id]}`)
      if (request.url === '/api/probes/config' && id) configReads[id]++
      const result = await mf.dispatchFetch(`${origin}${request.url}`, { method: request.method, headers: request.headers, ...(body.length && { body }) })
      const content = Buffer.from(await result.arrayBuffer())
      if (request.url === '/api/probes/ingest' && result.status !== 200) ingestErrors.push({ status: result.status })
      // Forward the real remote interval unchanged. No accelerated or rewritten wire configuration.
      const headers = Object.fromEntries(result.headers)
      delete headers['content-length']
      response.writeHead(result.status, headers)
      response.end(content)
    } catch { response.writeHead(500); response.end('Local receiver fixture failed') }
  })
  await listen(proxy)
  await admin('login', 'POST', { password })
  const initial = await admin('config')
  assert.equal(initial.revision, 0)
  saved = await admin('config', 'PUT', {
    revision: initial.revision, probes: fallback.probes,
    notificationTemplates: [{ id: 'private-notification', name: 'Private notification fixture', type: 'webhook', webhook: { url: 'https://notification.example.invalid/fixture', payloadType: 'json', headers: { Authorization: secrets.notification }, payload: { text: '$MSG' } } }],
    monitors: [
      { id: 'fast', name: '60 second target', method: 'GET', target: targetURL('/fast'), intervalSeconds: 60, timeout: 1000, headers: { 'X-Fixture-Secret': secrets.p1 }, probes: ['p1'], notificationTemplateId: 'private-notification' },
      { id: 'slow', name: '120 second target', method: 'GET', target: targetURL('/slow'), intervalSeconds: 120, timeout: 1000, probes: ['p1'] },
      { id: 'defaults', name: 'Optional default target', method: 'GET', target: targetURL('/defaults'), headers: { 'X-Fixture-Secret': secrets.p1 }, probes: ['p1'] },
      { id: 'p2-only', name: 'Second probe private target', method: 'GET', target: targetURL('/p2-only'), headers: { 'X-Fixture-Secret': secrets.p2 }, probes: ['p2'] },
    ],
  })
  assert.equal(saved.revision, 1)
  const firstConfig = await probeConfig('p1')
  const secondConfig = await probeConfig('p2')
  assert.deepEqual(firstConfig.monitors.map(m => m.id).sort(), ['defaults', 'fast', 'slow'])
  assert.deepEqual(secondConfig.monitors.map(m => m.id), ['p2-only'])
  const defaults = firstConfig.monitors.find(m => m.id === 'defaults')
  assert.equal(defaults.intervalSeconds, 300)
  assert.equal(defaults.timeout, 5000)
  assert(!JSON.stringify(firstConfig).includes(secrets.p2))
  assert(!JSON.stringify(secondConfig).includes(secrets.p1))
  assert(!JSON.stringify(firstConfig).includes('/p2-only'))
  assert(!JSON.stringify(secondConfig).includes('/fast'))
  const first = launch('p1'), second = launch('p2')
  const processIDs = [first.pid, second.pid]
  await until(async () => (await rows()).length === 4, 'first independently assigned checks durably persisted')
  assert.equal(events('/fast').length, 1)
  assert.equal(events('/slow').length, 1)
  assert.equal(events('/defaults').length, 1)
  assert.equal(events('/p2-only').length, 1)
  assert.equal(events('/fast')[0].fixture, secrets.p1)
  assert.equal(events('/p2-only')[0].fixture, secrets.p2)
  console.log(JSON.stringify({ phase: 'real-cadence-wait', remoteIntervalsSeconds: [60, 120, 300], legacyCLIIntervalSeconds: 1, approximateWaitSeconds: 60 }))
  await until(() => events('/fast').length >= 2, 'a real 60 second remote target deadline', 72000)
  const fastGapSeconds = (events('/fast')[1].at - events('/fast')[0].at) / 1000
  assert(fastGapSeconds >= 59 && fastGapSeconds < 70, 'The fast target must respect a real 60 second cadence')
  assert.equal(events('/fast').length, 2)
  assert.equal(events('/slow').length, 1, 'The 120 second target must not share the 60 second cadence')
  assert.equal(events('/defaults').length, 1, 'The default 300 second target must not inherit --interval 1s')
  assert.equal(events('/p2-only').length, 1)
  await until(async () => (await rows()).filter(row => row.probe_id === 'p1' && row.monitor_id === 'fast').length === 2, 'the second cadence result persisted in D1')

  const changedAt = Date.now()
  saved = await admin('config', 'PUT', {
    ...saved,
    monitors: [
      ...saved.monitors.map(monitor => monitor.id === 'fast' ? { ...monitor, target: targetURL('/slow-response'), timeout: 100 } : monitor.id === 'slow' ? { ...monitor, target: targetURL('/moved-to-p2'), probes: ['p2'] } : monitor),
      { id: 'new-target', name: 'New remote target', method: 'GET', target: targetURL('/new-target'), intervalSeconds: 60, probes: ['p1'] },
    ],
  })
  assert.equal(saved.revision, 2)
  const changedFirst = await probeConfig('p1'), changedSecond = await probeConfig('p2')
  assert.deepEqual(changedFirst.monitors.map(m => m.id).sort(), ['defaults', 'fast', 'new-target'])
  assert.deepEqual(changedSecond.monitors.map(m => m.id).sort(), ['p2-only', 'slow'])
  assert.equal(changedFirst.monitors.find(m => m.id === 'fast').timeout, 100)
  await until(async () => {
    const firstCached = await cache('p1'), secondCached = await cache('p2')
    return firstCached?.monitors?.find(m => m.id === 'fast')?.target === targetURL('/slow-response') &&
      firstCached.monitors.find(m => m.id === 'fast').timeout === 100 &&
      firstCached.monitors.some(m => m.id === 'new-target') && !firstCached.monitors.some(m => m.id === 'slow') &&
      secondCached?.monitors?.some(m => m.id === 'slow' && m.target === targetURL('/moved-to-p2'))
  }, 'running probes fetch and cache central target changes without restarting')
  await until(async () => {
    const stored = await rows()
    return stored.some(row => row.probe_id === 'p1' && row.monitor_id === 'fast' && row.up === 0 && row.stage === 'http' && row.code === 'timeout') &&
      stored.some(row => row.probe_id === 'p1' && row.monitor_id === 'new-target' && row.up === 1) &&
      stored.some(row => row.probe_id === 'p2' && row.monitor_id === 'slow' && row.up === 1)
  }, 'new checks, reassignment and 100ms HTTP timeout persisted from existing processes')
  assert(events('/slow-response').length >= 1)
  assert(events('/new-target').length >= 1)
  assert(events('/moved-to-p2').length >= 1)
  assert(slowResponseAborts >= 1, 'The 100ms timeout must abort the 400ms response')
  assert.equal(events('/fast').length, 2, 'The changed target must stop using its old URL')
  assert.equal(events('/slow').length, 1)
  assert(first.exitCode === null && first.signalCode === null && second.exitCode === null && second.signalCode === null)
  assert.deepEqual([first.pid, second.pid], processIDs, 'Target changes must not restart either process')
  const summaryResponse = await mf.dispatchFetch(`${origin}/fixture/summaries`)
  assert.equal(summaryResponse.status, 200)
  const summaries = await summaryResponse.json()
  assert.equal(summaries.fast.status, 'down')
  assert.equal(summaries.fast.probes[0].stage, 'http')
  assert.equal(summaries.fast.probes[0].code, 'timeout')
  assert.equal(summaries['new-target'].status, 'up')
  assert.equal(summaries.slow.probes[0].id, 'p2')
  assert.equal(summaries.slow.status, 'up')
  const timeoutSample = (await rows()).find(row => row.probe_id === 'p1' && row.monitor_id === 'fast' && row.code === 'timeout')
  assert(timeoutSample.latency_ms >= 75 && timeoutSample.latency_ms < 750)
  assert.equal(ingestErrors.length, 0, 'Every uploaded result must be accepted')
  await stop(first)
  await stop(second)
  const persisted = await rows()
  assert.equal(new Set(persisted.map(row => `${row.probe_id}:${row.monitor_id}:${row.time}`)).size, persisted.length)
  report = {
    passed: true, realGoBinary: binary, binarySHA256, localMiniflareD1: true, authenticatedAdminLoginAndSave: true,
    actualRuntimeConfigAndProbeHandlers: true, configurationRevisions: [1, 2], independentProbes: 2,
    assignedTargetsAndSecretsIsolated: true, defaultIntervalSeconds: defaults.intervalSeconds, defaultTimeoutMs: defaults.timeout,
    legacyCLIIntervalSeconds: 1, wireIntervalsUnmodified: true, observedFastGapSeconds: Number(fastGapSeconds.toFixed(3)),
    cadenceWindowCounts: { interval60: 2, interval120: 1, default300: 1 },
    liveTargetURLAndTimeoutChangeWithoutRestart: true, newAndReassignedTargetsExecuted: true,
    removedAssignmentAbsentFromRunningProbeCache: true, targetHTTPResponseDelayMs: 400,
    configuredLiveTimeoutMs: 100, persistedTimeoutStage: timeoutSample.stage, persistedTimeoutCode: timeoutSample.code,
    persistedTimeoutLatencyMs: Number(timeoutSample.latency_ms.toFixed(3)), settingsRefreshRequests: configReads,
    changedResultsVisibleWithinSeconds: Number(((Date.now() - changedAt) / 1000).toFixed(3)),
    persistedSamples: persisted.length, durationSeconds: Number(((Date.now() - started) / 1000).toFixed(3)),
  }
} catch (error) {
  report = { passed: false, error: redact(error.message), durationSeconds: Number(((Date.now() - started) / 1000).toFixed(3)) }
  console.error(redact(error.stack))
  for (const details of processes.values()) if (details.logs) console.error(`Local probe ${details.id} diagnostics:\n${redact(details.logs)}`)
  process.exitCode = 1
} finally {
  for (const child of children) { try { await stop(child) } catch {} }
  for (const timer of responseTimers) clearTimeout(timer)
  await close(proxy)
  await close(target)
  await mf?.dispose()
  await rm(temporary, { recursive: true, force: true })
}
await mkdir(join(root, 'bin'), { recursive: true })
await writeFile(reportPath, JSON.stringify(report, null, 2) + '\n')
console.log(JSON.stringify(report, null, 2))
