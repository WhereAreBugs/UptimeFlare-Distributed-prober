# UptimeFlare Distributed Prober

Go 编写的跨平台在线性探针，配合改造后的 [UptimeFlare](https://github.com/lyc8503/UptimeFlare) 使用。支持 TCP、HTTP、HTTPS、TLS 证书有效期、直接或代理 ICMP、连接阶段归因、断网落盘补传、gzip 批量上传和可选 OpenTelemetry 指标。

本目录是**独立的探针 Git 仓库**；本次工作区中的 `UptimeFlare/` 是另一个独立 Git 仓库，已被本仓库忽略。服务端安装说明在该仓库的 `docs/external-probes.md`。探针不依赖服务端源码即可编译和运行。

## 启动只需要两个配置

服务端先分配探针令牌与监控目标，然后运行：

```sh
export LIGHT_PROBER_SERVER=https://your-status.pages.dev
export LIGHT_PROBER_TOKEN=your-independent-probe-token
./light-prober
```

探针身份、目标列表、每个目标的探测周期、HTTP 方法、超时、请求头、请求体、预期状态码和关键词均从服务端获取，无须在每台主机重复维护。显示名称默认由服务端根据探针公网出口 IP 的地理位置与 ASN 自动生成，名称留空使用默认，手动名称优先；不增加探针的 IP 查询请求。服务端也提供可分配的 Cloudflare 内置探针，无需部署 Go 程序。令牌由服务端映射为稳定探针 ID，无须配置主机名或 ID。每个探针使用独立令牌和独立数据目录。

Windows PowerShell：

```powershell
$env:LIGHT_PROBER_SERVER = 'https://your-status.pages.dev'
$env:LIGHT_PROBER_TOKEN = 'your-independent-probe-token'
.\light-prober.exe
```

默认行为：

| 项目 | 默认值 |
| --- | --- |
| 探测间隔 | 每个目标独立调度，服务端默认 5 分钟，范围 60–86400 秒；不重叠执行、不补跑错过的周期 |
| 批量推送 | 默认 5 分钟，上限为当前最短目标周期；启动时恢复积压，首个新结果尽快上传 |
| 服务端配置刷新 | 5 分钟 |
| 并发探测 | 4，最多 32 |
| 单批样本 | 最多 200 |
| 请求压缩 | gzip，低 CPU 的 BestSpeed |
| 单目标超时 | 服务端指定；默认 5 秒，最长 120 秒 |
| 本地数据库容量 | 1 GiB，包含待上传队列、近期历史、页和索引开销；预留写事务余量 |
| 本地页面 | `127.0.0.1:9187`，只读、无外部资源依赖 |
| OpenTelemetry | 关闭 |

`./light-prober --help` 列出全部选项。服务端地址默认要求 HTTPS；本地联调才使用 `--allow-insecure`。令牌通过环境变量读取，避免出现在命令行参数中。

服务端监控配置的 `intervalSeconds` 使用整数秒，`timeout` 使用整数毫秒，例如 `"intervalSeconds": 60, "timeout": 5000`。刷新配置后，新目标立即开始探测，周期变化会重新计算对应目标的下一次执行时间，无须重启进程。保留的 `--interval` 默认为 5 分钟，仅供没有 `intervalSeconds` 的旧配置缓存使用；服务端明确下发的周期优先。超时大于周期时，该目标仍只运行一次检查，结束后跳过错过的周期。

## 保存与补传

每次完成探测，先同步提交至 bbolt 队列，再允许上传。服务端必须返回与批次内容对应的 `batch_id` 和样本数量，探针才同步删除对应条目。HTTP 成功但确认内容不匹配，也不会删除。传输失败采用 15 秒起的指数退避和随机抖动，最大约 5 分钟；支持 `Retry-After`。每轮恢复最多发送 10 批，仍有积压则稍后继续。

服务器以 `(probe_id, monitor_id, time)` 去重，重复上传不会重复计数；更新最新状态时只接受更晚的探测时间。保留完整样本，再按 UTC 的 5 分钟窗口建立汇总，因此短暂失败的阶段和原因不会因平均值而消失。`time` 为独立目标开始探测的 Unix 秒。每个目标的时间游标随结果一起持久化，确认并清空队列后仍保留；同秒快速重启或系统时钟回退时，等待真实时间超过游标后才再次检查该目标。

默认数据目录是操作系统用户配置目录下的 `light-prober`，也可通过 `--data-dir` 明确指定。目录内保存 `queue.db` 与 `config.json`。网络不可用时继续使用最近一次成功获取的配置；第一次上线且没有缓存时，等待服务端恢复后再开始探测。401/403 会暂停使用旧配置的探测。

队列绑定服务端地址和探针身份，防止更换令牌或地址后把旧结果记到其他探针。第二个进程无法同时打开相同队列。每条记录有版本与 CRC32C 校验，损坏记录不会被跳过或静默删除。

数据库容量上限为 1 GiB（1024 MiB），默认待确认负载预算也提高为 1024 MiB。写入前为 bbolt 的页、索引和写事务保留余量，因而可用负载会低于上限。容量耗尽会暂停采集，已有数据保留，补传和本地页面继续工作；腾出足够空间后自动恢复。磁盘写入失败会报错并停止进程。bbolt 会复用已释放页，文件不会自动缩小。网络故障不会造成已落盘结果丢失；磁盘损坏或长期容量不足无法保证继续采集。

Linux/macOS 使用目录 0700、文件 0600，配置更新同步文件并在重命名后同步目录。Windows 文件模式不等同于 ACL，应将数据放在仅服务账户可访问的目录；Windows 不提供与 Unix 相同的目录同步保证，缺失身份缓存时会停止启动而保留队列。

移除监控目标或取消探针分配前，先让该探针补传完积压。服务器拒绝未分配目标的数据；探针会保留被拒绝的批次，恢复原分配后可以继续补传。不要删除积压文件来绕过问题。

## 故障阶段

| stage | 含义 | 典型 code |
| --- | --- | --- |
| `dns` | DNS 解析 | `not_found`, `timeout` |
| `tcp` | TCP 建连 | `refused`, `reset`, `unreachable`, `timeout` |
| `tls` | TLS 握手、证书验证或临近过期 | `certificate`, `expiring`, `timeout` |
| `icmp` | ICMP Echo 请求与响应 | `unreachable`, `timeout`, `canceled` |
| `proxy` | 代理连接、鉴权、响应协议 | `refused`, `certificate`, `status`, `timeout`, `invalid_response` |
| `http` | 请求发送、等待响应、状态码校验 | `timeout`, `status`, `closed` |
| `body` | 响应读取与关键词校验 | `keyword`, `too_large`, `timeout` |
| `configuration` | 目标、请求配置或 ICMP 权限错误 | `target`, `method`, `header`, `keyword`, `permission`, `unsupported` |
| `unknown` | 无法准确定位 | `unknown` |

错误消息使用固定、安全的描述，不上传目标 URL、鉴权头、响应正文或原始错误中的凭据。Go `httptrace` 的回调受锁保护，避免并发拨号或重用连接后的重试误报阶段。HTTPS 使用系统信任根，验证证书；HTTP 不跟随重定向，需要显式允许 3xx 状态码或配置最终地址。

响应关键词使用流式 KMP 扫描，复用 32 KiB 缓冲区，扫描上限严格小于 1 MiB；关键词长度最多 4 KiB。未配置关键词时只限量读取正文以复用连接。请求头上限 64 KiB、连接池和并发数均有界。TCP 不需要 ICMP 权限或管理员权限。

服务端把“探针未上报/数据过期”显示为 unknown，与目标不可达区分。全部预期探针可达才显示 up；全部明确失败显示 down；结果混合或部分缺失显示 degraded。每个目标连续两倍探测周期无新结果就过期。

## 证书与 ICMP

服务端监控配置 `method: "SSL_CERT"` 使用 `https://host[:port]` 目标，只完成 TLS 握手，不发送 HTTP 请求。系统信任链、主机名、有效期均需通过校验；`certificateExpiryDays` 为 0–365 的整数，默认 14，距离过期不超过该窗口时以 `tls/expiring` 报告失败。0 关闭提前预警，过期或无效证书仍失败。样本保留 `certificate_expires_at`（Unix 秒）和 `certificate_days_remaining`（带小数，可为负值），包括对端已提供证书的验证失败。

`method: "ICMP_PING"` 的目标是主机名或裸 IP，不带 URL 或端口。主机名先解析 DNS，优先 IPv4；也支持 IPv6 地址。Linux/macOS 使用每次检查独立的免 root ICMP 数据报套接字和 16 字节随机载荷，确认来源及回包载荷，不调用外部 `ping` 程序。Windows 使用系统 IP Helper API；取消时等待当前原生调用返回，最长受剩余目标超时约束。FreeBSD 等其他平台通过代理执行 ICMP。样本的 `icmp_latency_ms` 是 Echo 往返时间，`latency_ms` 还包含解析及代理传输开销。平台接口依据 [Go ICMP 文档](https://pkg.go.dev/golang.org/x/net/icmp) 和 [Windows API](https://learn.microsoft.com/en-us/windows/win32/api/icmpapi/nf-icmpapi-icmpsendecho)。

Linux 的免 root ICMP 需要运行用户至少一个组落在 `net.ipv4.ping_group_range` 范围内；内核默认 `1 0` 不允许任何组。检查服务账户与 `sysctl net.ipv4.ping_group_range`，不要仅以 root 下 `ping` 成功判断 `DynamicUser` 服务可用。需要收紧权限时，创建专用固定 GID 的组，为服务增加 `SupplementaryGroups=该组名`，把 sysctl 设置为允许该 GID 的范围并持久化，再在服务账户下验证。此实现不尝试需要 `CAP_NET_RAW` 的原始套接字；权限不足明确显示 `configuration/permission`，可改用有权限的代理。Docker 也需允许容器 GID 65532。配置语义见 [Linux 内核文档](https://docs.kernel.org/networking/ip-sysctl.html#ping-group-range)。

`icmpProxyURL` 设为 HTTPS `/v1/check` 端点可委托 ICMP；`checkProxy` 对所有方法生效。代理鉴权统一使用 `checkProxyHeaders`；HTTP 目标鉴权仍放 `headers`，彼此独立。专用 `icmpProxyURL` 在没有 `checkProxyHeaders` 时兼容旧 `headers` 鉴权，ICMP 没有 HTTP 目标头，委托 JSON 会移除这些鉴权头；通用 `checkProxy` 不使用该旧字段回退。Go 支持 HTTP(S) 代理；`worker://`、`globalping://` 是服务端 Cloudflare 内置探针的执行选项。`checkProxyFallback: true` 仅在代理连接或协议失败时回退到该目标的常规检查路径；ICMP 配置了 `icmpProxyURL` 时，常规路径使用该专用代理，否则执行本机 ICMP。代理返回明确目标失败时不回退。返回的探针身份与默认名称继续取本探针出口 IP/ASN，代理执行地点由代理 URL 与其配置的 location label 标识。

部署通用代理：

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o check-proxy ./cmd/check-proxy
export LIGHT_PROBER_CHECK_PROXY_TOKEN='replace-with-a-random-token-of-at-least-16-characters'
./check-proxy --listen 127.0.0.1:8081 --location singapore --concurrency 4
```

使用 HTTPS 反向代理暴露 `/v1/check`，或提供 `--tls-cert`、`--tls-key` 让程序直接监听 HTTPS。私有 CA 场景可选 `--ca-file /path/to/roots.pem`，将这些根追加至系统信任根，仍执行证书与主机名验证。配置对应监控：

```json
{
  "id": "example-icmp",
  "method": "ICMP_PING",
  "target": "example.com",
  "timeout": 5000,
  "icmpProxyURL": "https://proxy.example/v1/check",
  "checkProxyHeaders": {"Authorization": "Bearer your-independent-proxy-token"}
}
```

代理接受 `POST /v1/check` 原始监控 JSON，支持 HTTP、TCP、SSL_CERT、ICMP_PING；兼容 `POST /v1/ping` 的 `{"target":"host","timeout_ms":5000}`，也接受毫秒 `timeout`。回应 `{location,status:{up,ping,err,stage?,code?,...}}`，附带证书/ICMP 样本字段。`/v1/ping` 还附带旧格式的平面 `up`、`latency_ms` 等字段，Go 的 `icmpProxyURL` 也兼容已有平面响应代理。Bearer 鉴权为必需，默认并发 4、上限 32，请求体最多 1 MiB、响应头受限、目标超时最长 120 秒；满负载返回 503 与 `Retry-After: 1`。代理清除收到的所有委托字段再执行检查，避免递归请求；不落盘或启动遥测后台。systemd 示例为 `deploy/check-proxy.service` 与 `deploy/check-proxy.env.example`。

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
| `probe.checks`, `probe.check.duration` | 探测数、延迟，按 method、up 与 stage 区分 |
| `probe.icmp.duration`, `probe.certificate.remaining` | ICMP RTT（ms）与证书剩余有效期（天），仅有对应样本时记录 |
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

构建脚本输出 Linux amd64/arm64/ARMv7、macOS amd64/arm64、Windows amd64/arm64、FreeBSD amd64 两种探针版本及通用 check-proxy。CI 在 Linux/macOS/Windows 上执行测试，生产部署另验证了两台 Linux amd64 实机；其余架构的编译成功不能代替实际目标运行验收。

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

`node scripts/check-proxy-worker-smoke.mjs` 使用真实 Worker Runtime 调用独立 Go 代理，检查可信 TLS、到期预警、ICMP 两种协议、代理鉴权失败与 D1 日汇总/可选样本字段；它自动创建并清理临时测试证书和进程。先编译 `bin/check-proxy-smoke` 并安装服务端 Worker 依赖。

`python3 scripts/check-proxy-fixture.py --self-test` 启动真实代理二进制及 HTTP/TLS 目标，验证可信证书、过期窗口和 ICMP 协议；仅联调脚本需要 Python/OpenSSL。使用 `--state-file /tmp/check-proxy-fixture.json` 可保持 fixture 运行，端点与临时令牌只写入该 0600 文件，供 Worker 网络联调读取，退出时删除。

服务端执行 `npx @cloudflare/next-on-pages` 后，可通过 `node scripts/pages-smoke.mjs ./UptimeFlare` 检查实际 Pages 构建产物的密钥绑定、鉴权、网页配置管理和 gzip 路由。资源测量脚本 `node scripts/resource-smoke.mjs` 适用于 Linux/macOS，使用本地测试目标。生产数据验收与这些本地测试分别记录在 `docs/validation.md`。

## 发布与部署管理

公开仓库：`WhereAreBugs/UptimeFlare-Distributed-prober`，服务端：`WhereAreBugs/UptimeFlare-Distributed`。GitHub Actions 在 Linux/macOS/Windows 验证探针并产出跨平台二进制；服务端自动部署使用其仓库的 Actions Secrets。

日常修改监控目标可通过已部署状态页的 `/admin` 登录完成；操作方法、实机路径、日志和升级步骤见 [运维说明](docs/operations.md)。

本地 `python3 scripts/configure-deployment.py cloudflare` 通过隐藏输入保存部署凭据到被忽略的 `.deployment/cloudflare.json`，权限 0600；`admin` 生成并保留随机管理员密码与会话密钥；`telemetry` 保存独立遥测鉴权。所有真实凭据均不得提交到仓库。

macOS 的 TUN 可能拦截 SSH；`scripts/ssh-physical.py` 可作为 SSH ProxyCommand 绑定指定物理网卡，不改变系统全局路由。仅用于部署机，不影响跨平台探针本身。

## 探针本地页面

启动后访问 `http://127.0.0.1:9187`。页面显示缓存的注册名称、地区/ASN、版本、系统、启动/配置同步时间，以及积压条数、待上传负载、数据库文件大小、队首采样时间、最近 ACK 和重试状态。开启目标排在暂停目标之前，两类目标内部保留配置顺序，排序在分页前完成。展开一个目标时加载与主站相同口径的 12 小时五分钟色带、90 天每日色带，以及可切换的 12 小时平均延迟／90 天可用率折线。桌面保留独立色块，手机合并连续同色区间，点击后只在当前页面展示时段、可达性、平均延迟。

历史存入 `queue.db` 的独立滚动桶，与队列 Append 在同一个事务中提交。ACK 只删除待上传记录，不删除历史。每个目标最多保留 145 个五分钟桶、90 个 UTC 日桶及最后结果，最多 500 个目标；暂停目标保留历史，取消分配后清理对应显示历史。无样本时显示未知，不推断失败；可用率按实际检测次数计算，延迟只统计成功检查，失败或混合五分钟桶让延迟折线断开。

升级时在容量余量允许的情况下，利用已有五分钟历史和最近 90 天待上传样本回填日历史，避免重叠记录重复计数；已确认删除且没有本地历史的样本无法重建。旧混合桶无法分离成功延迟时保留检测/失败次数，并将无法恢复的延迟留空。此后每次采集都保留日历史，仍受同一个 1 GiB 数据库预算约束。所有页面数据均来自本机，不上传队列统计，也不新增云端存储。

远程服务器可用 SSH 转发，无需开放公网端口：

```sh
ssh -N -L 9187:127.0.0.1:9187 root@45.192.249.191
# 另一台使用不同本地端口：
ssh -N -L 9188:127.0.0.1:9187 root@45.207.35.75
```

对应访问 `http://127.0.0.1:9187` 或 `http://127.0.0.1:9188`。`--web-listen ''` 关闭页面；监听其他 IP 必须设置至少 16 字符的独立 `LIGHT_PROBER_WEB_PASSWORD`，浏览器 HTTP Basic Auth 用户名任意。页面不接受修改请求，不显示内部 ID、令牌、请求头/体或带敏感参数的目标 URL。默认监听回环地址；外部监听应通过已有 HTTPS 代理访问。

`--max-queue-mib` 可降低待上传负载预算，取值 1–1024；数据库总预算仍为 1 GiB。页面没有第三方脚本、字体或遥测请求。
