#!/usr/bin/env python3
"""Local real-process fixture for Worker -> Go proxy protocol integration.

Requires Python/OpenSSL only for tests. The serving mode writes endpoints and a
temporary random bearer token to a mode-0600 state file, never to stdout. The Go
proxy trusts the generated TLS target through its --ca-file option. Send SIGTERM to
close all servers, child processes, certificates and the private state file.
"""

import argparse
import http.server
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request


class Target(http.server.BaseHTTPRequestHandler):
    def handle(self):
        # SSL_CERT deliberately closes immediately after the verified handshake.
        try:
            super().handle()
        except (ConnectionResetError, BrokenPipeError, ssl.SSLError):
            pass

    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"healthy")

    def log_message(self, *_args):
        pass


def request(state, path, monitor, authenticated=True):
    headers = {"Authorization": "Bearer " + state["token"]} if authenticated else {}
    req = urllib.request.Request(state["proxy_url"] + path, json.dumps(monitor).encode(), headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=3) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default="bin/check-proxy-smoke")
    parser.add_argument("--state-file", help="required serving-mode private JSON path")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if not args.self_test and not args.state_file:
        parser.error("serving mode requires --state-file")
    stopped = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda _signal, _frame: stopped.set())
    servers = []
    process = None
    state_file = Path(args.state_file) if args.state_file else None
    state_created = False
    try:
        with tempfile.TemporaryDirectory(prefix="check-proxy-fixture-") as directory:
            os.chmod(directory, 0o700)
            cert = Path(directory) / "cert.pem"
            key = Path(directory) / "key.pem"
            subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-sha256", "-days", "30", "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1,DNS:localhost", "-addext", "extendedKeyUsage=serverAuth", "-keyout", str(key), "-out", str(cert)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            os.chmod(key, 0o600)
            for secure in (False, True):
                server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Target)
                if secure:
                    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
                    context.load_cert_chain(cert, key)
                    server.socket = context.wrap_socket(server.socket, server_side=True)
                threading.Thread(target=server.serve_forever, daemon=True).start()
                servers.append(server)
            with socket.socket() as port_finder:
                port_finder.bind(("127.0.0.1", 0))
                port = port_finder.getsockname()[1]
            token = secrets.token_urlsafe(32)
            env = os.environ.copy()
            env["LIGHT_PROBER_CHECK_PROXY_TOKEN"] = token
            process = subprocess.Popen([str(Path(args.binary).resolve()), "--listen", f"127.0.0.1:{port}", "--location", "fixture-location", "--concurrency", "2", "--max-timeout", "2s", "--ca-file", str(cert)], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            state = {"proxy_url": f"http://127.0.0.1:{port}", "token": token, "http_target": f"http://127.0.0.1:{servers[0].server_address[1]}", "tls_target": f"https://127.0.0.1:{servers[1].server_address[1]}", "tcp_target": f"127.0.0.1:{servers[0].server_address[1]}", "icmp_target": "127.0.0.1", "ca_file": str(cert)}
            deadline = time.monotonic() + 5
            while True:
                try:
                    code, _ = request(state, "/v1/check", {}, False)
                    if code == 401:
                        break
                except urllib.error.URLError:
                    pass
                if time.monotonic() >= deadline or process.poll() is not None:
                    raise RuntimeError("check-proxy fixture did not start")
                stopped.wait(0.05)
            if args.self_test:
                for method, target in (("GET", state["http_target"]), ("TCP_PING", state["tcp_target"]), ("SSL_CERT", state["tls_target"]), ("ICMP_PING", state["icmp_target"])):
                    code, response = request(state, "/v1/check", {"method": method, "target": target, "timeout": 1000})
                    assert code == 200 and response["status"]["up"], method
                    if method == "SSL_CERT":
                        assert 29 < response["status"]["certificate_days_remaining"] <= 30
                code, response = request(state, "/v1/check", {"method": "SSL_CERT", "target": state["tls_target"], "timeout": 1000, "certificateExpiryDays": 31})
                assert code == 200 and not response["status"]["up"] and response["status"]["stage"] == "tls" and response["status"]["code"] == "expiring"
                for field in ("timeout_ms", "timeout"):
                    code, response = request(state, "/v1/ping", {"target": "127.0.0.1", field: 1000})
                    assert code == 200 and response["up"] and response["status"]["up"] and response["latency_ms"] >= 0
                print("check-proxy actual CLI fixture PASS: HTTP, TCP, trusted TLS certificate, expiry warning, ICMP, both legacy timeout fields, dual response, Bearer auth", flush=True)
            else:
                # Exclusive creation prevents overwriting an unrelated fixture.
                fd = os.open(state_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                state_created = True
                with os.fdopen(fd, "w") as output:
                    json.dump(state, output)
                print(json.dumps({"ready": True, "state_file": str(state_file)}), flush=True)
                while not stopped.wait(0.2):
                    if process.poll() is not None:
                        raise RuntimeError("check-proxy child stopped")
    finally:
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        for server in servers:
            server.shutdown()
            server.server_close()
        if state_created:
            state_file.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
