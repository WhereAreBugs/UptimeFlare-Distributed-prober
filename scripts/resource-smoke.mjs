// Reproducible local resource smoke; no external targets or collector.
import { createServer } from 'node:http'
import { spawn, execFileSync } from 'node:child_process'
import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { gunzipSync } from 'node:zlib'
import { fileURLToPath } from 'node:url'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const platform = process.platform === 'darwin' ? 'darwin' : 'linux'
const arch = process.arch === 'arm64' ? 'arm64' : 'amd64'
const report = { platform: `${platform}/${arch}`, monitors: 16, intervalSeconds: 1, telemetry: false, measurements: [] }
const results = new Set()
let wireBytes = 0
let plainBytes = 0
const server = createServer(async (request, response) => {
  if (request.url === '/target') { response.writeHead(204); response.end(); return }
  response.setHeader('Content-Type', 'application/json')
  if (request.url === '/api/probes/config') {
    const base = `http://127.0.0.1:${server.address().port}`
    response.end(JSON.stringify({ version: 1, probe_id: 'resource-smoke', monitors:
      Array.from({ length: report.monitors }, (_, i) => ({ id: `target-${i}`, method: 'GET', target: `${base}/target` })) }))
    return
  }
  const chunks = []
  for await (const chunk of request) chunks.push(chunk)
  const encoded = Buffer.concat(chunks)
  const data = request.headers['content-encoding'] === 'gzip' ? gunzipSync(encoded) : encoded
  wireBytes += encoded.length; plainBytes += data.length
  const batch = JSON.parse(data)
  for (const sample of batch.results) results.add(`${sample.monitor_id}:${sample.time}`)
  response.end(JSON.stringify({ batch_id: batch.batch_id, accepted: batch.results.length }))
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
try {
  for (const flavor of ['standard', 'nootel']) {
    results.clear(); wireBytes = 0; plainBytes = 0
    const dataDir = await mkdtemp(join(tmpdir(), 'light-prober-resource-'))
    const binary = resolve(root, `bin/light-prober-${platform}-${arch}-${flavor}`)
    const child = spawn(binary, ['--allow-insecure', '--interval', '1s', '--flush-interval', '1s', '--data-dir', dataDir], {
      env: { ...process.env, LIGHT_PROBER_SERVER: `http://127.0.0.1:${server.address().port}`, LIGHT_PROBER_TOKEN: 'resource-smoke-token' }, stdio: ['ignore', 'ignore', 'pipe'],
    })
    const logs = []; child.stderr.on('data', value => logs.push(value.toString()))
    const exited = new Promise(resolve => child.once('exit', (code, signal) => resolve({ code, signal })))
    let maxRSSKiB = 0; let processCPU = ''
    try {
      for (let i = 0; i < 16; i++) {
        await new Promise(resolve => setTimeout(resolve, 500))
        if (child.exitCode !== null) throw new Error(logs.join(''))
        const [rss, cpu] = execFileSync('ps', ['-o', 'rss=,time=', '-p', String(child.pid)], { encoding: 'utf8' }).trim().split(/\s+/)
        maxRSSKiB = Math.max(maxRSSKiB, Number(rss)); processCPU = cpu
      }
      child.kill('SIGTERM'); const exit = await exited
      if (exit.code !== 0) throw new Error(logs.join(''))
      report.measurements.push({ flavor, seconds: 8, maxRSSKiB, processCPU, uniqueSamples: results.size, plainBytes, wireBytes, compressedRatio: Number((wireBytes / plainBytes).toFixed(3)) })
    } finally { if (child.exitCode === null) child.kill('SIGKILL'); await exited; await rm(dataDir, { recursive: true, force: true }) }
  }
  await writeFile(resolve(root, 'bin/resource-report.json'), JSON.stringify(report, null, 2)+'\n')
  console.log(JSON.stringify(report, null, 2))
} finally { await new Promise(resolve => server.close(resolve)) }
