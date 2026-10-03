#!/usr/bin/env python3
"""Save deployment credentials locally without echoing or shell history."""

import argparse
import getpass
import json
import os
from pathlib import Path
import re
import secrets
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("kind", choices=("cloudflare", "github", "telemetry", "admin"))
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent / ".deployment"
    root.mkdir(mode=0o700, exist_ok=True)
    root.chmod(0o700)

    if args.kind == 'admin':
        destination = root / 'admin.json'
        if destination.exists():
            print(f"已有管理凭据，保留：{destination}")
            return
        values = {"password": secrets.token_urlsafe(24), "session_secret": secrets.token_urlsafe(48)}
    elif args.kind == "cloudflare":
        account = input("Cloudflare Account ID（32 位，可留空自动查询）: ").strip()
        if account and not re.fullmatch(r"[a-fA-F0-9]{32}", account):
            parser.error("Account ID 应为 32 位十六进制字符")
        token = getpass.getpass("Cloudflare API Token（输入不显示）: ").strip()
        if not token or any(c.isspace() for c in token):
            parser.error("Token 不能为空或含空白")
        values = {"account_id": account, "api_token": token}
    elif args.kind == "github":
        token = getpass.getpass("GitHub Token（输入不显示）: ").strip()
        if not token or any(c.isspace() for c in token):
            parser.error("Token 不能为空或含空白")
        values = {"api_token": token}
    else:
        endpoint = input("OTLP metrics endpoint [https://oobs.mac.catxxp123.top:9999/api/default/v1/metrics]: ").strip()
        endpoint = endpoint or "https://oobs.mac.catxxp123.top:9999/api/default/v1/metrics"
        if not endpoint.startswith("https://"):
            parser.error("遥测地址应使用 HTTPS")
        auth = getpass.getpass("OpenObserve Authorization 完整值（输入不显示，含 Basic 前缀）: ").strip()
        if not auth or "\n" in auth or "\r" in auth:
            parser.error("Authorization 不能为空或包含换行")
        values = {"metrics_endpoint": endpoint, "headers": {"Authorization": auth, "stream-name": "default"}}

    fd, temporary = tempfile.mkstemp(prefix=".credential-", dir=root)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w") as output:
            json.dump(values, output)
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        destination = root / (args.kind + ".json")
        os.replace(temporary, destination)
        print(f"已保存：{destination}（目录 0700、文件 0600，Git 已忽略）")
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


if __name__ == "__main__":
    main()
