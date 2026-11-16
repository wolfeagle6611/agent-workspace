# ADR 0010: 宿主机出口网关 + 双层网络强制

Status: accepted（本项目决策）

## 背景

沙箱里跑 agent 生成的代码。代码不可信，逃逸与数据外泄都要按事故处理。

现有仓库没有任何出网实现：没有 `http.Client`，没有代理，没有 netns。这块是新增面。

取法对象是 `cloudflare/proxy-everything`：一个在沙箱网络里跑的 sidecar，用 TPROXY 把出网流量重定向到本地，再用 HTTP `CONNECT` 连到外部网关。它只解决“把流量送到网关”，不解决“怎么判”。

## 决策

**每宿主机一个网关进程，每沙箱一个专属 Unix Domain Socket（L1 Socket Path Binding）。** 网关在宿主侧运行，不在 guest 内。每个沙箱在注册时由网关创建专属的数据面 Unix Domain Socket（`/run/egress/sandboxes/<sandboxId>/connect.sock`，以所在目录只读挂载入沙箱或映射自 Firecracker vsock UDS）及对应的临时根 CA（私钥存宿主沙箱目录，不进沙箱；网关重启后重新加载，沙箱重新建联即可）。网关从哪个专属 socket `Accept()` 连接，就天然确定其 `(sandboxId, sandboxCA)` 身份，不信任报文自报家门。控制面统一下发策略，但数据面不跨机器。

**guest 内跑 sidecar，关闭 TLS 拦截与内置 DNS。** `-tls-intercept` 与 `-dns-enabled` 置假。这样 sidecar 不嗅 SNI、不终止 TLS，只做 TCP 字节中继：收到 `CONNECT IP:port` 后网关先做 `IP:port` 初判并回 `200 OK` 建起字节隧道，再由网关 peek ClientHello 与终止 TLS，CA 私钥不出宿主（见 ADR 0012）。DNS（UDP/TCP 53）不经 sidecar TPROXY，由 `/etc/resolv.conf` 与 guest `DNAT` 直达宿主网关（网关 IP 在 sidecar 豁免网段内）。

数据链固定为：

```
guest app ──▶ TPROXY(iptables) ──▶ sidecar ──▶ CONNECT IP:port ──▶ 宿主网关 ──▶ 目标
                                                                     │
                                                         四道判定点 + header 改写
```

**两层强制，缺一不可。**

| 层 | 位置 | 规则 |
| --- | --- | --- |
| 第一层 | guest 内 iptables | TCP（除网关豁免网段外）重定向到 sidecar；UDP/TCP 53 `DNAT` 到宿主网关 `:53`；其余 UDP 与 ICMP `DROP` |
| 第二层 | 宿主（UDS 挂载 + nftables） | 每个沙箱只暴露其专属网关 Unix Socket（L1 Socket Path Binding）；每个 tap 的 DNS 只能到网关端点；任何直连公网的路由不存在 |

第二层是承重的。guest 内是 root，能 `iptables -F`，所以第一层只是便利，不是边界。

**四道判定点，共用一张策略表。**

1. 网关的 DNS 端点。
2. `CONNECT` 的 `IP:port`。
3. 域名提取与校验：443 端口取网关 peek ClientHello 得到的 SNI（并在 TLS 终止后与 HTTP `Host` 头严格比对，不一致即拒，防 Domain Fronting）；80 端口取明文 HTTP `Host` 头（非 HTTP 或缺 `Host` 即拒，且 80 端口严禁凭据注入）。
4. 网关按 `SNI/Host` 自行解析出的上游 IP（防 DNS rebinding 与客户端伪造 `CONNECT` 目标 IP；网关向上游拨号只连自己解析出的公网 IP，不信任客户端传来的 IP）。

冲突时以最严的一处为准。只拦 DNS 会被自定义解析器绕过；只判域名会被“白名单域名解析到内网 IP”绕过；只判 SNI 会被无 SNI 的连接绕过。

**TPROXY 只给 IP，不给域名。** `CONNECT` 里填的是 `conn.LocalAddr()`，所以域名只能从 443 的 ClientHello SNI 或 80 的明文 HTTP `Host` 取。443 无 SNI 或 80 无 `Host` 的连接按 IP 判定，默认拒绝。

**端口面只有 80 与 443。** 其余端口在网关直接 RST。非 80/443 端口没有可靠的域名提取与协议边界，放行等于放行未知目标。

**不支持 HTTP/3。** sidecar 只重定向 `-p tcp`，`UDP 443` 不被重定向。宿主层显式 `DROP` 全部 UDP（发往网关 `:53` 的除外），避免 QUIC 直连绕过。文档写明客户端会回落到 HTTP/2。

**默认策略是默认放公网 + 黑名单。** 这一条与“白名单”相反，是本项目自己的选择，理由见“后果”。

**每个 HTTP 请求重新查表。** 已建立的隧道保持，但每条请求都过策略。在途吊销即时生效。

**失败关闭。** 网关进程停、Redis 不可达、配置加载失败，一律不放行。没有直连降级。

**配置来源是手动文件，重启生效。** 启动日志打印配置内容哈希，供人工比对。改完忘重启时旧策略继续生效，这是已知代价。

## 放弃的方案

**应用层 `HTTP_PROXY`。** 放弃原因：可被 `--noproxy`、静态二进制、非 HTTP 协议绕过。零信任不能用可绕过的策略。

**只做 guest 内一层强制。** 放弃原因：guest 是 root，规则可清。

**每个沙箱一个网关。** 放弃原因：策略与密钥散到 N 处，吊销要挨个改。

**中心一个网关（早期草案）。** 放弃原因：Firecracker 的 guest 只能到宿主，跨机绕路没有收益；中心化让全体沙箱的出网挤一个进程。见 ADR 0014 的取代记录。

**开启 `-tls-intercept`。** 放弃原因：它把临时 CA 私钥写进 `/ca/ca.key`，即不可信侧。见 ADR 0012。

**v1 做 IPv6 双栈。** 放弃原因：多一条绕过路径与一套规则；v1 关闭 IPv6。

## 后果

- 外泄既无拦截也无记录。审计延后（ADR 0014 记录），Q22 的残余风险由此成立：默认放公网、无补偿控制、无检测。这条必须写进用户文档，不能只留在 ADR。
- 拒绝表现为 **TCP RST**，不是 HTTP 403。验收要断言“连接被重置”，不能断言状态码。
- `git+ssh`、非标准端口的服务全部失败。需要时走 HTTPS 远端。
- guest 内核必须编入 `CONFIG_NETFILTER_XT_TARGET_TPROXY`、`CONFIG_NETFILTER_XT_MATCH_SOCKET`、`CONFIG_IP_TRANSPARENT`。构建期断言，运行期不修。
- 网关是单进程单点：崩溃 = 本机全部沙箱断网。这是失败关闭的代价。
- 策略变更需要重启网关，重启会重置全部隧道。

## 重新评估的触发条件

1. 需要审计或 DLP：回到本文补数据面记录点。
2. 需要放非 80/443 端口：先做“域名到端口”的白名单，不能靠端口放行。
3. 默认放公网出现真实外泄事故：回到白名单。
4. 需要 HTTP/3：等于换掉 sidecar 的传输形状，不是加一条规则。
