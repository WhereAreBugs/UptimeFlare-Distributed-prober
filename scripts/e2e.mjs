#!/usr/bin/env node
/** Real Go -> gzip HTTP -> Cloudflare Worker -> D1 crash/replay smoke test. */
import assert from 'node:assert/strict'
import { spawn, execFileSync } from 'node:child_process'
import { createServer } from 'node:http'
import { once } from 'node:events'
import { mkdir, mkdtemp, readFile, writeFile, rm, access } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve, dirname } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { gunzipSync } from 'node:zlib'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const serverDir = resolve(process.argv[2] || join(root, 'UptimeFlare'))
const binary = resolve(process.argv[3] || join(root, 'bin/light-prober-smoke'))
const { Miniflare } = await import(pathToFileURL(join(serverDir, 'worker/node_modules/miniflare/dist/src/index.js')))
const { build } = await import(pathToFileURL(join(serverDir, 'worker/node_modules/esbuild/lib/main.js')))
const temporary = await mkdtemp(join(tmpdir(), 'light-prober-e2e-'))
const helperSource = join(root, 'bin/e2e-queue-inspect.go')
const children = new Set()
const tokens = { p1: 'e2e-probe-one-independent-token-123456', p2: 'e2e-probe-two-independent-token-123456' }
const logs = new Map()
let mf, proxy, target
let offline = false, loseNextAck = false, lostAck = null, replayObserved = false
let targetRequests = 0, gzipBatches = 0, wireBytes = 0, plainBytes = 0
const committedKeys = new Set()
const ingestErrors = []
const started = Date.now()
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))

async function until(condition, description, timeout = 45000) {
  const deadline = Date.now() + timeout
  while (Date.now() < deadline) {
    if (await condition()) return
    await sleep(100)
  }
  throw new Error(`Timed out: ${description}`)
}
async function listen(server) { server.listen(0, '127.0.0.1'); await once(server, 'listening'); return server.address().port }
async function close(server) { if (!server?.listening) return; server.closeAllConnections(); await new Promise(resolve => server.close(resolve)) }
function launch(id) {
  const child = spawn(binary, ['--server', `http://127.0.0.1:${proxy.address().port}`, '--allow-insecure', '--data-dir', join(temporary, id), '--interval', '1s', '--flush-interval', '1s', '--config-interval', '1s'], {
    cwd: root, env: { ...process.env, LIGHT_PROBER_TOKEN: tokens[id], OTEL_SDK_DISABLED: 'true' }, stdio: ['ignore', 'pipe', 'pipe'],
  })
  children.add(child)
  logs.set(child, '')
  for (const output of [child.stdout, child.stderr]) output.on('data', data => logs.set(child, (logs.get(child) + data.toString()).slice(-12000)))
  child.on('error', error => { logs.set(child, logs.get(child) + error.message) })
  return child
}
async function stop(child, signal = 'SIGTERM') {
  if (child.exitCode !== null || child.signalCode !== null) return
  const exited = once(child, 'exit')
  child.kill(signal)
  let timer
  try {
    await Promise.race([exited, new Promise((_, reject) => { timer = setTimeout(() => { child.kill('SIGKILL'); reject(new Error('Probe shutdown timed out')) }, 10000) })])
  } finally { clearTimeout(timer) }
  children.delete(child)
}
function inspect(id) {
  return JSON.parse(execFileSync(join(temporary, 'queue-inspect'), [join(temporary, id, 'queue.db')], { encoding: 'utf8', cwd: root }))
}
const sampleKey = (id, result) => `${id}:${result.monitor_id}:${result.time}`

try {
  await access(binary)
  await mkdir(join(root, 'bin'), { recursive: true })
  await writeFile(helperSource, `package main
import("encoding/json";"os";"light-prober/internal/spool")
func main(){q,e:=spool.Open(os.Args[1],1<<30);if e!=nil{panic(e)};defer q.Close();n,b,e:=q.Stats();if e!=nil{panic(e)};r,e:=q.Peek(200);if e!=nil{panic(e)};json.NewEncoder(os.Stdout).Encode(struct{Count uint64 \`json:"count"\`;Bytes int64 \`json:"bytes"\`;Entries []spool.Entry \`json:"entries"\`}{n,b,r})}
`)
  const goEnvironment = { ...process.env, GOCACHE: join(root, 'bin/e2e-go-cache') }
  delete goEnvironment.GOROOT
  delete goEnvironment.GOPATH
  execFileSync('go', ['build', '-o', join(temporary, 'queue-inspect'), helperSource], { cwd: root, env: goEnvironment, stdio: 'pipe', timeout: 45000 })
  await rm(helperSource)

  target = createServer((req, res) => { targetRequests++; res.writeHead(200, { 'Content-Type': 'text/plain' }); res.end('ready') })
  const targetPort = await listen(target)
  const closedPortServer = createServer()
  const closedPort = await listen(closedPortServer)
  await close(closedPortServer)
  const monitors = [
    { id: 'http', name: 'HTTP ready', method: 'GET', target: `http://127.0.0.1:${targetPort}`, timeout: 1000, responseKeyword: 'ready', probes: ['p1', 'p2'] },
    { id: 'tcp', name: 'TCP refused', method: 'TCP_PING', target: `127.0.0.1:${closedPort}`, timeout: 1000, probes: ['p1', 'p2'] },
  ]
  const entrypoint = `import {handleProbeRequest,getProbeSummaries} from ${JSON.stringify(join(serverDir, 'worker/src/probes.ts'))};
const monitors=${JSON.stringify(monitors)};
export default {async fetch(request,env){if(new URL(request.url).pathname==='/summaries')return new Response(JSON.stringify(await getProbeSummaries(env,monitors,[{id:'p1',name:'First'},{id:'p2',name:'Second'}])));return handleProbeRequest(request,env,monitors)}};`
  const bundled = await build({ stdin: { contents: entrypoint, resolveDir: serverDir }, bundle: true, write: false, format: 'esm', platform: 'browser', target: 'es2022' })
  mf = new Miniflare({ modules: true, script: bundled.outputFiles[0].text, compatibilityDate: '2025-04-02', d1Databases: ['UPTIMEFLARE_D1'], bindings: { PROBE_TOKENS: JSON.stringify(tokens) } })
  const db = await mf.getD1Database('UPTIMEFLARE_D1')
  const schema = await readFile(join(serverDir, 'init.sql'), 'utf8')
  for (const sql of schema.split(';').filter(sql => sql.trim())) await db.prepare(sql).run()

  proxy = createServer(async (req, res) => {
    try {
      const chunks = []
      for await (const chunk of req) chunks.push(chunk)
      const body = Buffer.concat(chunks)
      if (offline) { res.writeHead(503); res.end('offline smoke window'); return }
      let batch, id
      if (req.url === '/api/probes/ingest') {
        id = Object.keys(tokens).find(id => req.headers.authorization === `Bearer ${tokens[id]}`)
        const decoded = req.headers['content-encoding'] === 'gzip' ? gunzipSync(body) : body
        batch = JSON.parse(decoded)
        if (req.headers['content-encoding'] === 'gzip') gzipBatches++
        wireBytes += body.length
        plainBytes += decoded.length
      }
      const response = await mf.dispatchFetch(`https://receiver.test${req.url}`, { method: req.method, headers: req.headers, ...(body.length && { body }) })
      let content = Buffer.from(await response.arrayBuffer())
      // This crash/replay test accelerates only the legacy fallback cadence.
      // Validate the real receiver defaults before emulating an older cached config;
      // remote interval scheduling is covered separately by scheduler tests.
      if (req.url === '/api/probes/config' && response.status === 200) {
        const config = JSON.parse(content)
        for (const monitor of config.monitors) {
          assert.equal(monitor.intervalSeconds, 300)
          delete monitor.intervalSeconds
        }
        content = Buffer.from(JSON.stringify(config))
      }
      if (batch && response.status !== 200) ingestErrors.push({ status: response.status, body: content.toString() })
      if (batch && response.status === 200) {
        const keys = batch.results.map(result => sampleKey(id, result))
        for (const key of keys) committedKeys.add(key)
        if (lostAck && keys.some(key => lostAck.keys.includes(key))) replayObserved = true
        if (loseNextAck && id === 'p1') {
          loseNextAck = false
          lostAck = { batchId: batch.batch_id, keys }
          res.writeHead(503); res.end('committed ACK intentionally lost'); return
        }
      }
      const headers = Object.fromEntries(response.headers)
      delete headers['content-length']
      res.writeHead(response.status, headers)
      res.end(content)
    } catch (error) { res.writeHead(500); res.end(error.message) }
  })
  await listen(proxy)

  let first = launch('p1')
  await until(async () => { try { await access(join(temporary, 'p1/config.json')); return true } catch { return false } }, 'initial authenticated config cache', 10000)
  const beforeOffline = targetRequests
  offline = true
  await until(() => targetRequests >= beforeOffline + 4, 'offline target checks continue', 10000)
  await stop(first, 'SIGKILL')
  const crashQueue = inspect('p1')
  assert(crashQueue.count >= 6, 'Offline results must survive SIGKILL on disk')
  assert.equal(crashQueue.entries.length, crashQueue.count)

  const cachedStartRequests = targetRequests
  first = launch('p1')
  await until(() => targetRequests >= cachedStartRequests + 2, 'cached target config runs after offline restart', 10000)
  await stop(first, 'SIGKILL')
  const recoveredQueue = inspect('p1')
  assert(recoveredQueue.count > crashQueue.count, 'Restart must preserve old results and add cached offline checks')
  const recoveryKeys = recoveredQueue.entries.map(entry => sampleKey('p1', { monitor_id: entry.Result.monitor_id, time: entry.Result.time }))

  offline = false
  loseNextAck = true
  first = launch('p1')
  const second = launch('p2')
  await until(() => lostAck !== null, 'a successful persisted batch has its ACK lost', 10000)
  await until(() => replayObserved, 'Go retries persisted but unacknowledged samples', 45000)
  await until(async () => {
    const response = await mf.dispatchFetch('https://receiver.test/summaries')
    const summaries = await response.json()
    return summaries.http?.status === 'up' && summaries.http.probes.every(p => p.status === 'up') &&
      summaries.tcp?.status === 'down' && summaries.tcp.probes.every(p => p.stage === 'tcp')
  }, 'two independent probes read back with classified failures', 10000)
  await stop(first)
  await stop(second)
  const queuesAfterDrain = { p1: inspect('p1'), p2: inspect('p2') }
  assert.equal(queuesAfterDrain.p1.count, 0, 'Recovered queue drains only after matching durable ACK')
  assert.equal(queuesAfterDrain.p2.count, 0)
  for (const key of recoveryKeys) assert(committedKeys.has(key), `Recovered sample was never committed: ${key}`)
  const rows = await db.prepare('SELECT probe_id,monitor_id,time FROM probe_samples').all()
  const storedKeys = new Set(rows.results.map(row => `${row.probe_id}:${row.monitor_id}:${row.time}`))
  assert.equal(storedKeys.size, rows.results.length, 'D1 contains no duplicate sample identities')
  assert.equal(storedKeys.size, committedKeys.size, 'Every accepted identity is durable and counted once')
  for (const key of recoveryKeys) assert(storedKeys.has(key), `Crash-recovered sample missing from D1: ${key}`)
  for (const key of lostAck.keys) assert(storedKeys.has(key), `ACK-loss sample missing from D1: ${key}`)
  const summaries = await (await mf.dispatchFetch('https://receiver.test/summaries')).json()
  const summaryChecks = Object.values(summaries).flatMap(m => m.probes).reduce((sum, p) => sum + p.checks, 0)
  const historyChecks = Object.values(summaries).flatMap(m => m.probes).flatMap(p => p.history).reduce((sum, bucket) => sum + bucket.checks, 0)
  assert.equal(summaryChecks, storedKeys.size, 'Cumulative totals are replay-safe')
  assert.equal(historyChecks, storedKeys.size, 'Five-minute rollups are replay-safe')
  assert(Object.values(summaries).flatMap(m => m.probes).filter(p => p.status === 'down').every(p => p.recentFailures.length && p.failureStages.tcp > 0))
  assert(gzipBatches > 0 && wireBytes < plainBytes, 'Real Go uploads must use gzip and reduce wire bytes')
  const report = {
    passed: true, durationSeconds: Number(((Date.now() - started) / 1000).toFixed(2)),
    realGoBinary: binary, localMiniflareD1: true, independentProbes: 2,
    receiverDefaultIntervalVerified: true, acceleratedLegacyFallbackCadence: true,
    offlineCrashQueueResults: crashQueue.count, cachedRestartQueueResults: recoveredQueue.count,
    persistedSamples: storedKeys.size, lostAckReplayDeduplicated: true,
    recoveredQueuesEmpty: true, aggregateHttpStatus: summaries.http.status, aggregateTcpStatus: summaries.tcp.status,
    failureStage: 'tcp', gzipBatches, wireBytes, uncompressedBytes: plainBytes,
    compressionRatio: Number((wireBytes / plainBytes).toFixed(3)),
  }
  await writeFile(join(root, 'bin/e2e-report.json'), JSON.stringify(report, null, 2) + '\n')
  console.log(JSON.stringify(report, null, 2))
} catch (error) {
  console.error(error.stack)
  if (ingestErrors.length) console.error(JSON.stringify({ ingestErrors }))
  for (const [child, output] of logs) if (output) console.error(`Probe process ${child.pid}:\n${output}`)
  process.exitCode = 1
} finally {
  for (const child of children) { try { await stop(child, 'SIGKILL') } catch {} }
  await close(proxy)
  await close(target)
  await mf?.dispose()
  await rm(helperSource, { force: true })
  await rm(temporary, { recursive: true, force: true })
}
