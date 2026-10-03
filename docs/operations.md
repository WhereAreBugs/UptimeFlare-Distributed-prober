# 部署与日常维护

## 状态页与配置管理

- 公开页面：https://status.catxxp123.top/
- 网页管理：https://status.catxxp123.top/admin
- 服务端仓库：https://github.com/WhereAreBugs/UptimeFlare-Distributed
- 探针仓库：https://github.com/WhereAreBugs/UptimeFlare-Distributed-prober

管理密码由部署时随机生成，保存在部署机被 Git 忽略的 `.deployment/admin.json` 的 `password` 字段。生产密码和会话密钥同时保存在服务端仓库 Actions Secrets。真实凭据不得写入 issue、提交或公开文档。

登录后通过“监控目标”“探针”“通知模板”三个标签页配置。每个目标可独立设置检测间隔（可选，默认 300 秒）和超时（可选，默认 5000 毫秒），选择 HTTP/HTTPS 或 TCP 检测并分配执行探针。清空可选值使用默认值，已有显式配置保留。“探针”页显示各探针的目标分配与实际检测设置。“附加设置”可设置请求头、正文、预期状态码及关键词。统一保存后，独立探针默认在五分钟内获取新配置并自动调整调度，无需重新发布代码或重启。

首批独立探针内部标识为 `probe-1`、`probe-2`。另有无需令牌的内置 `cloudflare` 探针，可在“执行探针”中为指定目标选择，也可仅用 Cloudflare 检测。Cloudflare 每分钟由 Cron 唤醒，仅检查到期目标；独立目标周期支持 60–86400 秒，Cloudflare 执行时刻受一分钟 Cron 粒度限制。结果共用五分钟汇总、独立历史与阶段统计。独立探针的名称留空时，自动显示公网出口 IP 的国家、地区、城市与 ASN，手动名称优先；Cloudflare 默认显示最近执行的边缘节点与 AS13335。内部标识与令牌映射固定，界面只显示名称。目标与通知模板的标识自动分配、处理冲突，编辑时保留原标识及历史。多人同时编辑时版本冲突会拒绝保存，刷新页面后再修改。删除监控或取消分配前，先确认对应探针没有离线积压。

“通知模板”支持命名的 Webhook 配置：推送地址、请求方法、JSON/查询参数/表单、请求头、正文及超时。每个目标可选择一个模板或关闭通知，正文可使用 `$MSG`、`$MONITOR`、`$STATUS`、`$TIME`、`$REASON`、`$DURATION`。每分钟根据新鲜汇总结果判断全部有效探针失败与恢复，持久化事件并去重；失联与补传历史不单独触发告警。失败最多尝试 8 次，默认稳定的事件编号可供接收端去重。模板与鉴权信息仅在管理接口中读取，不下发给探针。

公开页面在折叠时显示汇总历史时间轴，可展开查看每台探针的独立结果。unknown 表示没有数据，或最新结果距离当前时间超过该目标检测间隔的两倍；默认 5 分钟周期对应 10 分钟离线阈值，不再另设离线超时。缺失或过期探针不参与汇总颜色或在线分母，汇总计数显示“可达的在线探针数/在线探针总数”。degraded 表示有数据的结果中同时包含成功和失败；全部无数据时显示灰色。探测失败会按 DNS、TCP、TLS、HTTP、正文等阶段展示。保持可见的状态页会按目标周期自动刷新，恢复前台时也刷新结果。

## Linux 探针

| 项目 | 路径或默认值 |
| --- | --- |
| 二进制 | `/usr/local/bin/light-prober` |
| systemd 服务 | `light-prober.service`，已设置开机自启 |
| 环境配置 | `/etc/light-prober.env`，仅 root 可读写 |
| 队列与配置缓存 | `/var/lib/light-prober/queue.db`、`config.json` |
| 检测 / 批量上传 / 配置刷新 | 按每个目标设置，默认 5 分钟 / 默认 5 分钟 / 5 分钟 |
| 遥测 | 已开启，1 分钟导出一次 OTLP metrics |

服务以 systemd 的临时用户运行；StateDirectory 在部分发行版显示为指向 `/var/lib/private/light-prober` 的链接，这是 systemd 的正常行为。

检查服务与近期日志：

```sh
systemctl status light-prober --no-pager
journalctl -u light-prober --since '15 minutes ago' --no-pager
systemctl show light-prober -p MemoryCurrent -p CPUUsageNSec
```

上传失败时队列保留并退避重试。短周期目标会把正常上传周期缩短到不大于最短检测周期，防止等待默认五分钟批次造成假失联；历史统计仍使用五分钟窗口。配置获取失败时可使用缓存中的独立目标周期继续采集；鉴权失败会暂停旧配置。不要删除队列绕过上传错误，先检查令牌、目标分配、磁盘和到状态页的连通性。

升级时从探针仓库 Actions 的 `binaries` 下载对应平台**标准版**，核对文件来源后上传到主机。保留环境文件与数据目录，再用同一文件系统的临时名称替换二进制：

```sh
install -m 0755 /path/to/new-binary /usr/local/bin/light-prober.next
mv /usr/local/bin/light-prober.next /usr/local/bin/light-prober
systemctl restart light-prober
systemctl status light-prober --no-pager
```

仓库推送会自动验证与构建探针；探针主机升级通过上述安装流程完成。服务端 `main` 推送会自动部署 Pages、Worker 和 D1；不会覆盖已经通过网页保存的配置。

## OpenObserve

探针发送性能、队列、检查与上传指标，使用 metrics 管道。当前 endpoint 为 `https://oobs.mac.catxxp123.top:9999/api/default/v1/metrics`，鉴权通过 `/etc/light-prober.env` 中的 `OTEL_EXPORTER_OTLP_HEADERS` 设置。该路径是 metrics 的完整 OTLP HTTP endpoint；仅配置 traces 管道无法接收这些指标。

查询时使用 `service.name=light-prober`，按 `probe.id` 区分真实探针。指标目录见 README；OpenObserve 可将名称中的点规范化为下划线。一次性部署检查使用不同的 `service.name=light-prober-deployment-verification`，可过滤排除。

关闭遥测需从 systemd 的 ExecStart 移除 `--telemetry --telemetry-interval 1m`，执行 `systemctl daemon-reload` 并重启服务；保留本地队列。默认关闭遥测的程序不会初始化 SDK 或产生导出请求。开启遥测的部署应使用标准版二进制，`nootel` 版不接受该开关。
