#!/usr/bin/env node
/** Real Worker -> authenticated Go proxy -> trusted TLS/ICMP -> D1 metadata and daily history. */
import assert from 'node:assert/strict'
import { readFile,writeFile,mkdtemp,rm } from 'node:fs/promises'
import { spawn } from 'node:child_process'
import { once } from 'node:events'
import { tmpdir } from 'node:os'
import { fileURLToPath } from 'node:url'
import { resolve,join,dirname } from 'node:path'
import { pathToFileURL } from 'node:url'
const root=resolve(dirname(fileURLToPath(import.meta.url)),'..'),serverDir=resolve(process.argv[2]||join(root,'UptimeFlare'))
const temporary=await mkdtemp(join(tmpdir(),'check-proxy-worker-'))
const stateFile=join(temporary,'fixture.json')
const child=spawn('python3',[join(root,'scripts/check-proxy-fixture.py'),'--state-file',stateFile],{cwd:root,stdio:['ignore','ignore','pipe']})
let fixture
const deadline=Date.now()+10000
try {
 while(!fixture) {try{fixture=JSON.parse(await readFile(stateFile,'utf8'))}catch{};if(child.exitCode!==null||Date.now()>deadline)throw new Error('Check proxy fixture did not start');if(!fixture)await new Promise(resolve=>setTimeout(resolve,50))}
} catch(error) {child.kill('SIGTERM');await rm(temporary,{recursive:true,force:true});throw error}
const {build}=await import(pathToFileURL(join(serverDir,'worker/node_modules/esbuild/lib/main.js')))
const {Miniflare}=await import(pathToFileURL(join(serverDir,'worker/node_modules/miniflare/dist/src/index.js')))
const monitors=[
 {id:'certificate',name:'Certificate healthy',method:'SSL_CERT',target:fixture.tls_target,probes:['cloudflare'],checkProxy:fixture.proxy_url+'/v1/check',checkProxyHeaders:{Authorization:'Bearer '+fixture.token}},
 {id:'expiring',name:'Certificate threshold',method:'SSL_CERT',target:fixture.tls_target,certificateExpiryDays:31,probes:['cloudflare'],checkProxy:fixture.proxy_url+'/v1/check',checkProxyHeaders:{Authorization:'Bearer '+fixture.token}},
 {id:'icmp',name:'ICMP proxy',method:'ICMP_PING',target:fixture.icmp_target,probes:['cloudflare'],icmpProxyURL:fixture.proxy_url+'/v1/ping',headers:{Authorization:'Bearer '+fixture.token}},
 {id:'icmp-generic',name:'Generic ICMP proxy',method:'ICMP_PING',target:fixture.icmp_target,probes:['cloudflare'],checkProxy:fixture.proxy_url+'/v1/check',checkProxyHeaders:{Authorization:'Bearer '+fixture.token}},
 {id:'unauthorized',name:'Incorrect proxy credential',method:'SSL_CERT',target:fixture.tls_target,probes:['cloudflare'],checkProxy:fixture.proxy_url+'/v1/check',checkProxyHeaders:{Authorization:'Bearer incorrect-credential-fixture'}},
]
for(const monitor of monitors) monitor.timeout=1000
const bundled=await build({stdin:{contents:`import {runCloudflareProbe} from ${JSON.stringify(join(serverDir,'worker/src/cloudflare-probe.ts'))}; import {getProbeSummaries} from ${JSON.stringify(join(serverDir,'worker/src/probes.ts'))}; const monitors=${JSON.stringify(monitors)}; export default{async fetch(request,env){for(const monitor of monitors) await runCloudflareProbe(env,[monitor],Math.floor(Date.now()/1000),'LOCAL'); return Response.json(await getProbeSummaries(env,monitors,[{id:'cloudflare'}]));}}`,resolveDir:serverDir},bundle:true,write:false,format:'esm',platform:'browser',target:'es2022',external:['cloudflare:sockets']})
const mf=new Miniflare({modules:true,script:bundled.outputFiles[0].text,compatibilityDate:'2025-04-02',compatibilityFlags:['nodejs_compat'],d1Databases:['UPTIMEFLARE_D1']})
try{
 const db=await mf.getD1Database('UPTIMEFLARE_D1')
 for(const sql of(await readFile(join(serverDir,'init.sql'),'utf8')).split(';').filter(s=>s.trim()))await db.prepare(sql).run()
 const response=await mf.dispatchFetch('https://worker.test/run');assert.equal(response.status,200)
 const results=await response.json()
 assert.equal(results.certificate.status,'up');assert(results.certificate.probes[0].certificateExpiresAt>Math.floor(Date.now()/1000));assert(results.certificate.probes[0].certificateDaysRemaining>29)
 assert.equal(results.expiring.status,'down');assert.equal(results.expiring.probes[0].stage,'tls');assert.equal(results.expiring.probes[0].code,'expiring')
 assert.equal(results.icmp.status,'up');assert(results.icmp.probes[0].icmpLatencyMs>=0)
 assert.equal(results['icmp-generic'].status,'up');assert(results['icmp-generic'].probes[0].icmpLatencyMs>=0)
 assert.equal(results.unauthorized.status,'down');assert.equal(results.unauthorized.probes[0].stage,'proxy')
 const rows=await db.prepare('SELECT probe_id,monitor_id,checks,failures FROM probe_days ORDER BY monitor_id').all();assert.equal(rows.results.length,5)
 const report={passed:true,actualGoProxy:true,realWorkerRuntime:true,realD1:true,trustedLocalTLS:true,expiryThreshold:true,legacyICMPProtocol:true,genericICMPProtocol:true,proxyAuthenticationFailurePhase:'proxy',sparseMetadataPersisted:true,dailyRows:rows.results.length}
 await writeFile(join(root,'bin/check-proxy-worker-smoke-report.json'),JSON.stringify(report,null,2)+'\n');console.log(JSON.stringify(report))
}finally{await mf.dispose();const exited=once(child,'exit');child.kill('SIGTERM');await exited;await rm(temporary,{recursive:true,force:true})}
