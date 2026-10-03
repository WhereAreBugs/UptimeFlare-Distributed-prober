# UptimeFlare Distributed Prober

Go 编写的跨平台在线性探针，配合改造后的 [UptimeFlare](https://github.com/lyc8503/UptimeFlare) 使用。支持 TCP、HTTP、HTTPS、连接阶段归因、断网落盘补传、gzip 批量上传和可选 OpenTelemetry 指标。

本目录是**独立的探针 Git 仓库**；本次工作区中的 `UptimeFlare/` 是另一个独立 Git 仓库，已被本仓库忽略。服务端安装说明在该仓库的 `docs/external-probes.md`。探针不依赖服务端源码即可编译和运行。

## 启动只需要两个配置

服务端先分配探针令牌与监控目标，然后运行：

```sh
export LIGHT_PROBER_SERVER=https://your-status.pages.dev
export LIGHT_PROBER_TOKEN=your-independent-probe-token
./light-prober
```

探针身份、目标列表、HTTP 方法、超时、请求头、请求体、预期状态码和关键词均从服务端获取，无须在每台主机重复维护。显示名称默认由服务端根据探针公网出口 IP 的地理位置与 ASN 自动生成，名称留空使用默认，手动名称优先；不增加探针的 IP 查询请求。服务端也提供可分配的 Cloudflare 内置探针，无需部署 Go 程序。令牌由服务端映射为稳定探针 ID，无须配置主机名或 ID。每个探针使用独立令牌和独立数据目录。

Windows PowerShell：

```powershell
$env:LIGHT_PROBER_SERVER = 'https://your-status.pages.dev'
$env:LIGHT_PROBER_TOKEN = 'your-independent-probe-token'
.\light-prober.exe
```

默认行为：

| 项目 | 默认值 |
| --- | --- |
| 探测间隔 | 1 分钟，不重叠执行、不补跑错过的轮次 |
| 批量推送 | 5 分钟；启动时先尝试恢复已有积压 |
| 服务端配置刷新 | 5 分钟 |
| 并发探测 | 4，最多 32 |
| 单批样本 | 最多 200 |
| 请求压缩 | gzip，低 CPU 的 BestSpeed |
| 单目标超时 | 服务端指定；默认 10 秒，最长 120 秒 |
| 本地队列容量 | 256 MiB 的待确认 JSON 负载，实际文件有页和索引开销 |
| OpenTelemetry | 关闭 |

`./light-prober --help` 列出全部选项。服务端地址默认要求 HTTPS；本地联调才使用 `--allow-insecure`。令牌通过环境变量读取，避免出现在命令行参数中。

## 保存与补传

每次完成探测，先同步提交至 bbolt 队列，再允许上传。服务端必须返回与批次内容对应的 `batch_id` 和样本数量，探针才同步删除对应条目。HTTP 成功但确认内容不匹配，也不会删除。传输失败采用 15 秒起的指数退避和随机抖动，最大约 5 分钟；支持 `Retry-After`。每轮恢复最多发送 10 批，仍有积压则稍后继续。

服务器以 `(probe_id, monitor_id, time)` 去重，重复上传不会重复计数；更新最新状态时只接受更晚的探测时间。保留完整样本，再按 UTC 的 5 分钟窗口建立汇总，因此短暂失败的阶段和原因不会因平均值而消失。`time` 为探测轮次开始的 Unix 秒，排队等候并发槽的目标属于同一轮次。

默认数据目录是操作系统用户配置目录下的 `light-prober`，也可通过 `--data-dir` 明确指定。目录内保存 `queue.db` 与 `config.json`。网络不可用时继续使用最近一次成功获取的配置；第一次上线且没有缓存时，等待服务端恢复后再开始探测。401/403 会暂停使用旧配置的探测。

队列绑定服务端地址和探针身份，防止更换令牌或地址后把旧结果记到其他探针。第二个进程无法同时打开相同队列。每条记录有版本与 CRC32C 校验，损坏记录不会被跳过或静默删除。

容量耗尽或磁盘写入失败会明确报错并停止探测，已有数据保留；不会用新结果覆盖旧结果。容量限制针对待确认负载，bbolt 文件会复用已释放页，文件不会自动缩小。需要按目标数量和预计离线时间配置容量，并保留文件系统余量。网络故障不会造成已落盘的结果丢失；磁盘损坏或长期容量不足无法保证继续采集。

Linux/macOS 使用目录 0700、文件 0600，配置更新同步文件并在重命名后同步目录。Windows 文件模式不等同于 ACL，应将数据放在仅服务账户可访问的目录；Windows 不提供与 Unix 相同的目录同步保证，缺失身份缓存时会停止启动而保留队列。

移除监控目标或取消探针分配前，先让该探针补传完积压。服务器拒绝未分配目标的数据；探针会保留被拒绝的批次，恢复原分配后可以继续补传。不要删除积压文件来绕过问题。

## 故障阶段

| stage | 含义 | 典型 code |
| --- | --- | --- |
| `dns` | DNS 解析 | `not_found`, `timeout` |
| `tcp` | TCP 建连 | `refused`, `reset`, `unreachable`, `timeout` |
| `tls` | TLS 握手或证书验证 | `certificate`, `timeout` |
| `http` | 请求发送、等待响应、状态码校验 | `timeout`, `status`, `closed` |
| `body` | 响应读取与关键词校验 | `keyword`, `too_large`, `timeout` |
| `configuration` | 目标或请求配置错误 | `target`, `method`, `header`, `keyword` |
| `unknown` | 无法准确定位 | `unknown` |

错误消息使用固定、安全的描述，不上传目标 URL、鉴权头、响应正文或原始错误中的凭据。Go `httptrace` 的回调受锁保护，避免并发拨号或重用连接后的重试误报阶段。HTTPS 使用系统信任根，验证证书；HTTP 不跟随重定向，需要显式允许 3xx 状态码或配置最终地址。

响应关键词使用流式 KMP 扫描，复用 32 KiB 缓冲区，扫描上限严格小于 1 MiB；关键词长度最多 4 KiB。未配置关键词时只限量读取正文以复用连接。请求头上限 64 KiB、连接池和并发数均有界。TCP 不需要 ICMP 权限或管理员权限。

服务端把“探针未上报/数据过期”显示为 unknown，与目标不可达区分。全部预期探针可达才显示 up；全部明确失败显示 down；结果混合或部分缺失显示 degraded。默认 15 分钟无新结果就过期。

## 可选遥测

```sh
export OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=https://collector.example/v1/metrics
# 若 Collector 需要鉴权：
export OTEL_EXPORTER_OTLP_HEADERS='Authorization=Bearer your-telemetry-token'
./light-prober --telemetry --telemetry-interval 1m
```

使用官方 OpenTelemetry Go SDK 的 OTLP HTTP/protobuf 导出器，gzip 压缩。Collector 由你配置，与状态页接收端分别配置。支持标准 `OTEL_EXPORTER_OTLP_*` 环境变量。默认不初始化 SDK、导出线程或遥测网络请求；开启后默认每分钟导出，最低间隔 10 秒，导出超时 5 秒，指标属性基数上限 64。

| 指标 | 用途 |
| --- | --- |
| `probe.checks`, `probe.check.duration` | 探测数、延迟，按 up 与 stage 区分 |
| `probe.uploads`, `probe.upload.duration`, `probe.upload.bytes` | 推送成功率、耗时与实际压缩后字节数 |
| `probe.queue.results`, `probe.queue.bytes` | 未确认积压 |
| `probe.config.failures` | 配置获取失败 |
| `probe.runtime.heap`, `probe.runtime.goroutines`, `probe.runtime.gc.cpu` | Go 堆、协程数、GC CPU 时间 |

指标自动附带服务端分配的 `probe.id`，区分多个探针；支持 `OTEL_RESOURCE_ATTRIBUTES` 定制部署标签。不在指标属性中放 URL、错误正文或监控 ID。遥测不成功不会阻断探测和补传。只需在线性功能时，可以构建 `nootel` 版本，彻底移除遥测 SDK；该版本收到 `--telemetry` 会明确报错。

## 构建、验证与部署

需要 Go 1.26；运行二进制不需要 Go、Node、数据库服务或 C 编译器。

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o light-prober ./cmd/light-prober
CGO_ENABLED=0 go build -tags nootel -trimpath -ldflags='-s -w' -o light-prober-minimal ./cmd/light-prober
go test -race ./cmd/... ./internal/...
go test -tags nootel ./cmd/... ./internal/...
go vet ./cmd/... ./internal/...
sh scripts/build.sh
```

构建脚本输出 Linux amd64/arm64/ARMv7、macOS amd64/arm64、Windows amd64/arm64、FreeBSD amd64 两种版本。CI 在 Linux/macOS/Windows 上执行测试，生产部署另验证了两台 Linux amd64 实机；其余架构的编译成功不能代替实际目标运行验收。

Linux systemd 示例在 `deploy/light-prober.service`，通过 `DynamicUser`、`StateDirectory` 和只读系统目录运行。把二进制放到 `/usr/local/bin/light-prober`，将 `deploy/light-prober.env.example` 复制成 `/etc/light-prober.env` 并设置真实值、0600 权限，再安装并启用 service。Windows 可通过任务计划程序在专用账户下运行，macOS 可通过 launchd 保持运行；普通进程版本无需平台专用服务依赖。

Docker：

```sh
docker build -t light-prober .
docker run --restart unless-stopped --env-file /path/to/light-prober.env \
  --mount type=bind,source=/path/to/probe-data,target=/data light-prober
```

镜像以 UID/GID 65532 运行，挂载目录需预先赋予该账户写权限。镜像内包含 CA 信任根，默认使用持久数据卷。多架构构建可使用 Docker Buildx。

在包含服务端源码与已安装 Worker 测试依赖的联合工作区，还可执行：

```sh
CGO_ENABLED=0 go build -o bin/light-prober-smoke ./cmd/light-prober
node scripts/e2e.mjs ./UptimeFlare
```

联调用真实 Go 进程和 Miniflare D1，覆盖断网、进程强制退出、缓存启动、丢失确认后的重传与多探针汇总。测试证据和本机性能测量见 `docs/validation.md`。

服务端执行 `npx @cloudflare/next-on-pages` 后，可通过 `node scripts/pages-smoke.mjs ./UptimeFlare` 检查实际 Pages 构建产物的密钥绑定、鉴权、网页配置管理和 gzip 路由。资源测量脚本 `node scripts/resource-smoke.mjs` 适用于 Linux/macOS，使用本地测试目标。生产数据验收与这些本地测试分别记录在 `docs/validation.md`。

## 发布与部署管理

公开仓库：`WhereAreBugs/UptimeFlare-Distributed-prober`，服务端：`WhereAreBugs/UptimeFlare-Distributed`。GitHub Actions 在 Linux/macOS/Windows 验证探针并产出跨平台二进制；服务端自动部署使用其仓库的 Actions Secrets。

日常修改监控目标可通过已部署状态页的 `/admin` 登录完成；操作方法、实机路径、日志和升级步骤见 [运维说明](docs/operations.md)。

本地 `python3 scripts/configure-deployment.py cloudflare` 通过隐藏输入保存部署凭据到被忽略的 `.deployment/cloudflare.json`，权限 0600；`admin` 生成并保留随机管理员密码与会话密钥；`telemetry` 保存独立遥测鉴权。所有真实凭据均不得提交到仓库。

macOS 的 TUN 可能拦截 SSH；`scripts/ssh-physical.py` 可作为 SSH ProxyCommand 绑定指定物理网卡，不改变系统全局路由。仅用于部署机，不影响跨平台探针本身。
