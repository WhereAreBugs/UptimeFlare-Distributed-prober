# 验证记录

日期：2026-10-03，Asia/Singapore。环境：Apple M2 / macOS arm64，Go 1.26.3，Node 24.3.0。服务端基于 UptimeFlare `a5670e51cbc167bf3610fce4d3389dd00141d729` 改造。

## 已通过

- Go 标准版：`go test -race ./cmd/... ./internal/...`，包括真实本机 DNS/TCP/HTTP/TLS、连接阶段超时、正文边界、持久化、损坏校验、锁、队列容量、重启与 ACK 验证。
- Go 最小版：`go test -tags nootel ./cmd/... ./internal/...`；`go vet` 通过。
- OpenTelemetry：本地 Collector 接收到 OTLP protobuf 与 gzip 指标，包含 `probe.id`，未包含监控 ID/错误正文；禁用路径没有 SDK。
- Worker：50 项测试通过，其中 26 项使用本地 Miniflare D1 验证探针、管理配置、内置 Cloudflare 与自动标签，24 项检查原生错误归因及监控流程；类型检查通过。
- 公共 API/前端逻辑：11 项测试通过；Next ESLint、TypeScript、生产构建与 Cloudflare Pages 打包通过。
- Worker 发布 dry-run 通过，产物约 86 KiB，gzip 约 21 KiB。
- 实际生成的 Pages Worker 产物在 Miniflare 中通过认证配置、401、gzip 解压后权限拒绝、405 和空 D1 状态检查，验证 `process.env` secret 桥接及 Next 路由；未授权写入数为 0。
- 浏览器检查实际组件：主机汇总、独立探针、阶段统计、五分钟历史与失败表格能展开；384px 视口未产生页面横向溢出，没有浏览器错误。
- 无 CGO 的标准版与最小版均成功构建 Linux amd64/arm64/ARMv7、macOS amd64/arm64、Windows amd64/arm64、FreeBSD amd64，共 16 个二进制。

## 完整进程联调

`node scripts/e2e.mjs ./UptimeFlare` 使用实际 Go 二进制、本地 Cloudflare Worker 运行时及 Miniflare D1；不是 mock 的内存存储。最后一次结果：

| 验证项 | 结果 |
| --- | --- |
| 断网后 SIGKILL 的队列 | 10 条仍在磁盘 |
| 断网缓存启动继续采集 | 重启后 14 条 |
| 确认响应丢失后重传 | 去重，未重复计数 |
| 所有积压身份 | 均出现在服务端持久记录 |
| 原始记录/累计统计/五分钟统计 | 均为 84 条 |
| 恢复并停止后两个本地队列 | 均为 0 |
| 两探针 HTTP 汇总 | up |
| 两探针关闭端口的 TCP 汇总 | down，阶段 tcp |
| 本次短批次流量 | 12,353 → 5,189 字节，减少 58.0% |

联调发现了同秒崩溃重启导致重复样本身份、整批被拒绝的问题。修复为队列原子保存最大已采集时间，即使 ACK 清空后也保留；重启或时钟回退后等待真实时间超过游标，不生成未来时间。单元测试与原故障联调均通过。

## 资源测量

`node scripts/resource-smoke.mjs`，16 个本地 HTTP 204 目标、每秒一轮、持续 8 秒，关闭遥测，每版 128 个独立样本。这是短时本机测量，不是生产容量承诺：

| 版本 | 峰值 RSS | 进程 CPU 时间 | 请求体字节 | gzip 字节 |
| --- | --- | --- | --- | --- |
| 标准版 | 23,952 KiB（23.4 MiB） | 0.13 秒 | 10,871 | 2,921 |
| nootel | 19,808 KiB（19.3 MiB） | 0.20 秒 | 10,849 | 2,936 |

gzip 在该场景减少约 73% 的请求体字节。测量不包含 TLS/HTTP 包头和 Collector 流量。默认一分钟检测、五分钟推送的运行频率比该测量低，实际占用取决于目标、响应大小、失败阶段与离线积压。

探测核心基准（本机独立运行）：复用本地 HTTP 约 36.3µs、6.5KiB/88 次分配；TCP 约 57.4µs、1.8KiB/31 次分配。持久化 Append+Peek+Ack 约 22.7ms，包含两次同步事务，受存储设备显著影响；没有通过关闭 fsync 来得到不可靠的性能数字。

## 生产部署验收

2026-10-03 已建立并推送两个公开仓库：

- [UptimeFlare-Distributed](https://github.com/WhereAreBugs/UptimeFlare-Distributed)：Cloudflare Pages、定时 Worker、D1，通过 Actions 自动部署。
- [UptimeFlare-Distributed-prober](https://github.com/WhereAreBugs/UptimeFlare-Distributed-prober)：Go 探针、三平台测试和 16 个无 CGO 构建。

[服务端部署](https://github.com/WhereAreBugs/UptimeFlare-Distributed/actions/runs/37104462898)与[服务端验证](https://github.com/WhereAreBugs/UptimeFlare-Distributed/actions/runs/37104462891)均成功；[探针 CI](https://github.com/WhereAreBugs/UptimeFlare-Distributed-prober/actions/runs/37103998407)在 Linux、macOS、Windows 上运行 race、nootel 测试与 vet，全部通过，跨平台构建产物也已上传。

[公开状态页](https://status.catxxp123.top/) 已绑定自定义域名。两台 Debian 13 / Linux amd64 实机通过 systemd `DynamicUser` 启动并设置开机自启，数据目录 `/var/lib/light-prober`，环境文件 0600，本地队列和缓存配置均为 0600。使用默认一分钟探测、五分钟批量上传，没有为验收缩短间隔。

首批真实验收直接查询生产 D1：两台探针各有 5 条测试站点记录，均成功；原始样本、累计 checks 与五分钟 bucket checks 均相等。公开 API 显示两台探针 fresh/up，浏览器可展开每台的延迟、累计检查及五分钟历史。后续部署保留管理配置 revision 1。

生产管理接口已验证匿名读取 401、正确密码登录、配置读取与保存、公开配置隐藏目标、注销。浏览器管理入口展示登录表单且无 console 错误。真实 Pages 构建产物的本地验收另覆盖安全 Cookie、跨来源写入拒绝、并发版本冲突以及探针获取修改后配置。

两台实机已开启一分钟一次的 gzip OTLP metrics 导出，运行日志未见导出错误。另在两台主机上以现有凭据运行同一遥测代码的一次性检查，使用独立 `service.name` 与 `probe.id`，非空指标导出均成功；未改动生产探针进程与数据队列。OpenObserve 查询 API 对提供的凭据返回 401，因此此处只确认导出被接收，尚未验证 OpenObserve 的持久化查询结果。

实机验收修复了 systemd 无 HOME 环境下过早读取配置目录的问题。Windows CI 修复了真实 Winsock 错误码归类，以及文件权限和计时粒度的测试假设。D1 测试按持久记录比较，排除每次查询变化的耗时元数据；保留了完整数据不变的断言。

启动后约六分钟，两台 systemd cgroup 的 MemoryCurrent 分别约 11.2 MiB、12.2 MiB，遥测开启；这是单目标短期观测，尚未建立长期生产资源曲线。FreeBSD 为交叉编译验证；Docker 示例尚未在生产运行。

## Cloudflare 内置探针与自动命名增补

2026-10-03 新增可分配的 `cloudflare` 探针，复用实际原生 HTTP/TCP 检查并写入与 Go 探针相同的 D1 历史。新增真实 Miniflare D1 测试覆盖混合汇总、Cloudflare 单独分配、未分配目标不检查、定时事件重复与乱序、禁止外部令牌冒充内置身份，以及认证后才更新默认标签、保留手动名称、缺失元数据保留旧标签、ASN 变化更新、无变化时不写入。正文检查另验证超时、1 MiB 上限和流取消。

50 项 Worker 与 11 项公共 API/汇总测试通过，类型检查、lint、生产 Pages 构建通过。实际 Pages 产物的 smoke 测试以固定平台 `cf` 元数据验证 `getOptionalRequestContext()` 的地理位置和 ASN 桥接、内置探针分配与公共名称；使用 Miniflare 的平台配置，未采用客户端自报请求头。

服务端提交 `2209d02` 的[验证流程](https://github.com/WhereAreBugs/UptimeFlare-Distributed/actions/runs/37124626652)和[部署流程](https://github.com/WhereAreBugs/UptimeFlare-Distributed/actions/runs/37124626661)均成功。上线后通过管理 API 将测试目标分配给 `probe-1`、`probe-2`、`cloudflare`，配置版本为 2；保留旧目标、历史与独立令牌。

生产 D1 直接查询已确认 Cloudflare cron 产生真实样本，三类探针的原始记录、累计 checks/failures 与五分钟桶总数一致。公开 API 与浏览器均显示 3/3 可达，独立探针自动名称为 `JP / Tokyo · AS61112` 与 `MO / Macau · AS61112`；Cloudflare 的最近节点在验收期间出现 IAD 与 SIN，默认名称为相应节点加 AS13335。Go 程序无需升级，不额外请求第三方地理位置服务。
## 2026-10-04 Webhook 通知模板与配置页

服务端新增可复用 Webhook 模板、目标选择、故障与恢复通知、D1 事件队列和失败重试。配置页隐藏内部标识，目标与模板自动分配唯一标识并保留现有历史；指定文案已精简。

60 个 Worker 测试及 15 个页面/API 测试通过。通知测试使用真实 Miniflare D1，并通过本地 HTTP 接收端验证 JSON、查询参数、表单、鉴权头与字符串替换；覆盖并发去重、原子回滚、失败重试及顺序、关闭通知取消排队、失联与旧补传不单独触发、重定向拒绝与超时。实际 next-on-pages 产物另验证模板保存、匿名拒绝、配置冲突、重复标识处理，以及私密通知配置不下发给探针或公开 API。

## 2026-10-04 远程配置与独立探测周期

每个目标支持可选 `intervalSeconds`（默认 300 秒，范围 60–86400 秒）和 `timeout`（默认 5000 毫秒）；离线阈值由该目标间隔的两倍计算。管理员保存配置后，已分配的探针自动获取更新。旧配置与队列兼容，显式超时值保留，旧的全局离线参数忽略；不重写现有配置版本。Cloudflare 定时器每分钟运行，仅执行到期目标。

Go race、nootel、vet 均通过；74 个 Worker 测试、18 个页面/API 测试、类型检查、lint、完整构建通过。最终 Pages 产物的 17 项实际运行时检查覆盖默认值、修改后配置、鉴权和私密配置隔离。调度测试覆盖独立周期、并发限额、配置更新唤醒、Cloudflare 并发租约和迟到执行者、事务回滚、原生监控状态写入的并发保护、各目标独立新鲜度边界。

`node scripts/remote-config-smoke.mjs ./UptimeFlare` 使用最终 Go 二进制、实际管理接口及 Worker/D1，耗时 62.75 秒。60 秒目标两次执行相隔 59.997 秒，120 秒和默认 300 秒目标均只执行一次；远端周期覆盖 CLI 的 1 秒回退值。真实保存配置后，新增目标、重新分配、URL 与 100 毫秒超时无需重启即生效，400 毫秒响应在约 101.5 毫秒被中止。此联调将配置拉取设为 1 秒用于加速验收，生产仍为默认 5 分钟。

断网进程联调再次通过：崩溃时磁盘保留 8 条，缓存启动继续积压到 12 条，恢复后 D1 持久化 80 条；确认响应丢失后的重传未重复计数，两个队列均清空。测试显式验证接收端默认 300 秒，仅在加速场景模拟旧协议省略周期；新协议真实周期由上述独立联调验证。22 个 gzip 批次的正文由 11923 字节降至 5401 字节。

服务端提交 [`b6798bf`](https://github.com/WhereAreBugs/UptimeFlare-Distributed/commit/b6798bf9a6090dba8122c7f644c6c5048c8a50ca) 的[部署](https://github.com/WhereAreBugs/UptimeFlare-Distributed/actions/runs/37147413534)与[验证](https://github.com/WhereAreBugs/UptimeFlare-Distributed/actions/runs/37147413538)成功。探针提交 [`2f773ff`](https://github.com/WhereAreBugs/UptimeFlare-Distributed-prober/commit/2f773ff45d40f4b3dd718ed13e207d2f25265639) 的[三平台测试与 16 个交叉构建](https://github.com/WhereAreBugs/UptimeFlare-Distributed-prober/actions/runs/37147415740)全部成功。

两台实机均已原子替换二进制并重启，版本为 `2f773ff`，SHA256 为 `abc44fc83f409668fe9afe24f5cc8fa8097eeeb27d9781d541e9d9b267521f24`；原二进制保留备份，队列、环境文件与缓存保留，systemd 运行与开机自启正常。启动日志确认遥测仍开启、默认周期与上传上限均为 300 秒，新进程未见 WARN/ERROR。认证配置从每台探针的实际网络出口获取，以保持地理位置自动命名正确。

2026-10-04 03:27（Asia/Singapore）直接查询生产 D1，`probe-1` 最近两个样本为 1791055618/1791055318，`probe-2` 为 1791055622/1791055322，Cloudflare 为 1791055500/1791055200，间隔均为 300 秒。原始样本、累计统计与五分钟桶的 checks/failures 一致；公开 API 三探针均 fresh/up，汇总 3/3。部署前后的保存配置哈希一致、revision 保持 3，测试目标原有显式 10000 毫秒超时保留。

生产浏览器确认可选周期和超时、各探针执行目标摘要、全局离线字段移除，以及现有标签页和汇总历史保留。未保存的草稿把周期改成 60 秒并清空超时后，各探针摘要显示 60 秒/5 秒；刷新丢弃草稿并恢复原有配置，未用测试值覆盖生产配置。
