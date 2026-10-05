#!/usr/bin/env python3
"""Real binary: offline dashboard, queue/ACK/restart, metadata and secret boundary."""
import argparse, gzip, json, os, signal, subprocess, tempfile, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.request import urlopen

root = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser()
parser.add_argument('--binary', default=str(root / 'bin/light-prober'))
parser.add_argument('--preview', action='store_true')
args = parser.parse_args()
state = {'uploads': False, 'config': True, 'status_requests': 0}
token = 'fictional-probe-token-for-local-dashboard'

class Receiver(BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def send(self, value, status=200):
        data = json.dumps(value).encode()
        self.send_response(status); self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(data))); self.end_headers(); self.wfile.write(data)
    def do_GET(self):
        if self.path == '/api/probes/config':
            if not state['config']: return self.send({}, 503)
            assert self.headers.get('Authorization') == 'Bearer ' + token
            target = 'http://127.0.0.1:' + str(self.server.server_port)
            monitors = [{'id': 'good', 'name': '可达测试', 'method': 'GET', 'target': target + '/good?credential=private-target', 'intervalSeconds': 300, 'headers': {'Authorization': 'private-header'}, 'timeout': 5000}, {'id': 'bad', 'name': '失败测试', 'method': 'GET', 'target': target + '/bad', 'intervalSeconds': 300, 'timeout': 5000}]
            display = [{k: m[k] for k in ['id', 'name', 'method', 'intervalSeconds', 'timeout']} for m in monitors]
            display.insert(0, {'id': 'paused', 'name': '暂停测试', 'method': 'GET', 'intervalSeconds': 300, 'timeout': 5000, 'paused': True})
            return self.send({'version': 1, 'probe_id': 'internal-probe', 'probe': {'name': 'Example / Tokyo · AS64500', 'location': 'Example / Tokyo'}, 'monitors': monitors, 'display_monitors': display})
        self.send({}, 503 if self.path == '/bad' else 200)
    def do_POST(self):
        if self.path == '/api/probes/status': state['status_requests'] += 1
        if self.path != '/api/probes/ingest': return self.send({}, 404)
        data = self.rfile.read(int(self.headers['Content-Length']))
        if self.headers.get('Content-Encoding') == 'gzip': data = gzip.decompress(data)
        batch = json.loads(data)
        if not state['uploads']: return self.send({}, 503)
        self.send({'batch_id': batch['batch_id'], 'accepted': len(batch['results'])})

server = ThreadingHTTPServer(('127.0.0.1', 0), Receiver)
threading.Thread(target=server.serve_forever, daemon=True).start()
temporary = tempfile.TemporaryDirectory(prefix='local-probe-dashboard-')
process = None
def start():
    global process
    process = subprocess.Popen([args.binary, '--server', f'http://127.0.0.1:{server.server_port}', '--allow-insecure', '--data-dir', temporary.name, '--web-listen', '127.0.0.1:0', '--config-interval', '1s', '--flush-interval', '1s'], env={**os.environ, 'LIGHT_PROBER_TOKEN': token, 'OTEL_SDK_DISABLED': 'true'}, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
    for _ in range(15):
        line = process.stderr.readline()
        value = json.loads(line)
        if value.get('msg') == 'local dashboard listening':
            stream = process.stderr
            def drain():
                for _ in stream: pass
            threading.Thread(target=drain, daemon=True).start()
            return 'http://' + value['address']
    raise AssertionError('Dashboard not listening')
def stop():
    if process and process.poll() is None:
        process.send_signal(signal.SIGTERM); process.wait(timeout=10)
        assert process.returncode == 0
def get(url):
    with urlopen(url, timeout=5) as response: return json.load(response)
def until(fn):
    deadline = time.monotonic() + 12
    while time.monotonic() < deadline:
        result = fn()
        if result: return result
        if process.poll() is not None: raise AssertionError('Probe exited')
        time.sleep(.1)
    raise AssertionError('Timed out waiting for real probe')
try:
    page = start()
    until(lambda: get(page + '/api/status')['status']['queue_count'] >= 2)
    info = get(page + '/api/status')
    assert info['registered'] and info['probe']['name'].endswith('AS64500')
    assert info['status']['database_limit'] == 1 << 30
    assert info['status']['database_bytes'] <= 1 << 30
    targets = get(page + '/api/monitors?page=1')
    assert targets['total'] == 3 and targets['monitors'][2]['paused']
    for path in ['/api/status', '/api/monitors', '/api/history?monitor=good', '/api/history?monitor=bad']:
        encoded = json.dumps(get(page + path))
        for secret in [token, 'private-header', 'private-target', 'internal-probe']: assert secret not in encoded
    assert get(page + '/api/history?monitor=good')['latest']['up']
    assert not get(page + '/api/history?monitor=bad')['latest']['up']
    state['uploads'] = True
    stop()  # Shutdown obtains a matching ACK; the history must remain.
    state['config'] = False
    page = start()  # No configuration network: cached registration/history work.
    info = get(page + '/api/status')
    assert info['registered'] and info['status']['last_config_at'] > 0
    assert info['status']['last_upload_at'] > 0
    assert get(page + '/api/history?monitor=good')['buckets']
    assert get(page + '/api/history?monitor=bad')['buckets']
    good_days = get(page + '/api/history?monitor=good')['daily_buckets']
    bad_days = get(page + '/api/history?monitor=bad')['daily_buckets']
    assert good_days and good_days[-1]['checks'] > 0 and good_days[-1]['failures'] == 0
    assert good_days[-1]['latency_checks'] == good_days[-1]['checks']
    assert bad_days and bad_days[-1]['failures'] == bad_days[-1]['checks']
    assert bad_days[-1]['latency_checks'] == 0 and bad_days[-1]['latency_sum'] == 0
    assert state['status_requests'] == 0
    assert Path(temporary.name, 'queue.db').stat().st_mode & 0o777 == 0o600
    print(json.dumps({'passed': True, 'realBinary': True, 'offlineCachedRegistration': True, 'historySurvivesAckAndRestart': True, 'dailyHistorySurvivesAckAndRestart': True, 'enabledTargetsFirst': True, 'databaseLimitGiB': 1, 'noCloudStatusRequests': True, **({'preview_url': page} if args.preview else {})}), flush=True)
    if args.preview:
        while True: time.sleep(1)
finally:
    stop(); server.shutdown(); server.server_close(); temporary.cleanup()
