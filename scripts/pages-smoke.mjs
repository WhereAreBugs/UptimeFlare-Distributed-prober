#!/usr/bin/env node
/** Smoke test the actual next-on-pages deployment artifact and its environment bridge. */
import assert from 'node:assert/strict'
import { readFile, access, writeFile, mkdir, readdir } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { gzipSync } from 'node:zlib'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const serverDir = resolve(process.argv[2] || join(root, 'UptimeFlare'))
const artifact = join(serverDir, '.vercel/output/static/_worker.js/index.js')
const { Miniflare } = await import(pathToFileURL(join(serverDir, 'worker/node_modules/miniflare/dist/src/index.js')))
const token = 'pages-smoke-independent-fixture-token-123456'
const password = 'pages-smoke-admin-password-123456789'
await access(artifact)
async function moduleFiles(directory) {
  const entries = await readdir(directory, { withFileTypes: true })
  const nested = await Promise.all(entries.map(entry => entry.isDirectory() ? moduleFiles(join(directory, entry.name)) : entry.name.endsWith('.js') ? [{ type: 'ESModule', path: join(directory, entry.name) }] : []))
  return nested.flat()
}
const modules = [{ type: 'ESModule', path: artifact }, ...(await moduleFiles(dirname(artifact))).filter(module => module.path !== artifact)]
const mf = new Miniflare({
  modules,
  modulesRoot: dirname(artifact),
  compatibilityDate: '2025-04-02',
  compatibilityFlags: ['nodejs_compat'],
  d1Databases: ['UPTIMEFLARE_D1'],
  bindings: { PROBE_TOKENS: JSON.stringify({ p1: token }), ADMIN_PASSWORD: password, ADMIN_SESSION_SECRET: 'pages-smoke-session-secret-at-least-32-characters' },
  serviceBindings: { ASSETS: () => new Response('Static asset omitted in API smoke test', { status: 404 }) },
})
const outcomes = []
async function check(path, init, status, assertion) {
  const response = await mf.dispatchFetch(`https://pages.test${path}`, init)
  const text = await response.text()
  assert.equal(response.status, status, `${path}: ${text.slice(0, 1000)}`)
  if (assertion) assertion(JSON.parse(text))
  outcomes.push({ path, method: init?.method || 'GET', status })
  return response
}
try {
  const db = await mf.getD1Database('UPTIMEFLARE_D1')
  const schema = await readFile(join(serverDir, 'init.sql'), 'utf8')
  for (const sql of schema.split(';').filter(sql => sql.trim())) await db.prepare(sql).run()
  await check('/api/probes/config', { headers: { Authorization: `Bearer ${token}` } }, 200, config => {
    assert.equal(config.version, 1)
    assert.equal(config.probe_id, 'p1')
    assert.deepEqual(config.monitors, [])
    assert(!JSON.stringify(config).includes(token))
  })
  await check('/api/probes/config', {}, 401)
  await check('/api/probes/ingest', { method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json', 'Content-Encoding': 'gzip' }, body: gzipSync(JSON.stringify({
    version: 1, batch_id: '0'.repeat(64), results: [{ monitor_id: 'foo_monitor', time: Math.floor(Date.now() / 1000), up: true, latency_ms: 1 }],
  })) }, 403, body => assert.equal(body.error, 'Probe is not assigned to this monitor'))
  await check('/api/probes/ingest', { headers: { Authorization: `Bearer ${token}` } }, 405)
  await check('/api/data', {}, 200, value => assert.equal(value.unknown, 1))
  await check('/api/admin/config', {}, 401)
  await check('/api/admin/login', { method: 'POST', headers: { 'Content-Type': 'application/json', Origin: 'https://evil.test' }, body: JSON.stringify({ password }) }, 403)
  const login = await check('/api/admin/login', { method: 'POST', headers: { 'Content-Type': 'application/json', Origin: 'https://pages.test' }, body: JSON.stringify({ password }) }, 200)
  const cookie = login.headers.get('Set-Cookie').split(';')[0]
  const config = {
    revision: 0, probes: [{ id: 'p1', name: 'Smoke probe' }], probeStaleAfterSeconds: 900,
    monitors: [{ id: 'smoke', name: 'Smoke target', method: 'GET', target: 'https://private-smoke.example', probes: ['p1'], headers: { Authorization: 'private-target-secret' } }],
  }
  await check('/api/admin/config', { method: 'PUT', headers: { 'Content-Type': 'application/json', Origin: 'https://pages.test', Cookie: cookie }, body: JSON.stringify(config) }, 200)
  await check('/api/admin/config', { method: 'PUT', headers: { 'Content-Type': 'application/json', Origin: 'https://pages.test', Cookie: cookie }, body: JSON.stringify(config) }, 409)
  await check('/api/probes/config', { headers: { Authorization: `Bearer ${token}` } }, 200, value => assert.equal(value.monitors[0].target, 'https://private-smoke.example'))
  await check('/api/data', {}, 200, value => {
    assert.equal(value.unknown, 1)
    assert(!JSON.stringify(value).includes('private-smoke'))
    assert(!JSON.stringify(value).includes('private-target-secret'))
  })
  const persisted = await db.prepare('SELECT COUNT(*) n FROM probe_samples').first()
  assert.equal(persisted.n, 0)
  const report = { passed: true, actualPagesArtifact: artifact, processEnvSecretBridge: true, middlewareAndApiRoutes: true, gzipDecompressedBeforeAssignmentValidation: true, realD1Binding: true, authenticatedAdminAndDynamicConfiguration: true, checks: outcomes }
  await mkdir(join(root, 'bin'), { recursive: true })
  await writeFile(join(root, 'bin/pages-smoke-report.json'), JSON.stringify(report, null, 2) + '\n')
  console.log(JSON.stringify(report, null, 2))
} finally {
  await mf.dispose()
}
