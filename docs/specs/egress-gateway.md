# Spec: 沙箱出口网关与幽灵 token 注入

状态：待发布（缺 issue tracker 配置）
标签：`ready-for-agent`（发布时套用）
相关 ADR：0010（出口网关）、0011（幽灵 token）、0013（Rust + Pingora 落地）、待写（临时 CA）

## Problem Statement

沙箱里跑 agent 生成的代码。代码不可信。

当前仓库没有任何出网实现：没有 `http.Client`，没有代理，没有 netns。沙箱一旦拿到网络，就有三条事故路径：

1. 代码直连任意域名，把 Workspace 内容发出去。
2. 为了让代码调用外部 API，只能把真密钥放进沙箱。沙箱被攻破即密钥泄露。
3. 沙箱 root 能清掉自己的防火墙规则，一条 guest 内的规则不是边界。

用户需要的是一条可运营的出口路径：代码能装依赖、能调 API，但密钥不进沙箱，出网必须过策略点，策略点不能被沙箱关掉。

## Solution

每个沙箱宿主跑一个**出口网关**。沙箱的出网流量全部经它。

- 沙箱内跑 sidecar（`proxy-everything`，关闭 TLS 拦截与内置 DNS），用 TPROXY 把 TCP 送到宿主网关；DNS（UDP/TCP 53）经 `/etc/resolv.conf` 与 guest `DNAT` 直达宿主网关（网关 IP 列入 sidecar 豁免网段）。
- 网关终止 TLS，看到明文，按域名判定，改写 header，再转发。
- 沙箱持有的不是真密钥，是**幽灵 token**。网关用幽灵 token 查表换真密钥。
- 强制分两层：guest 内 iptables 是第一层，宿主 nftables 是第二层。沙箱 root 清规则不影响第二层。
- 默认策略是默认放公网 + 黑名单（不是白名单）。这条残余风险由用户承担，见 ADR 0010。

## User Stories

### 沙箱里的代码

1. 作为沙箱里的代码，我想用普通环境变量拿到一个可用的 token，这样我不需要知道幽灵 token 这件事。
2. 作为沙箱里的代码，我想照常 `curl https://api.github.com`，这样现有脚本不用改。
3. 作为沙箱里的代码，我想照常 `npm install` / `pip install`，这样依赖能装。
4. 作为沙箱里的代码，我想在请求里带 `Authorization: Bearer <我的 token>`，这样我不需要特殊 header 名。
5. 作为沙箱里的代码，我想在无凭据的情况下访问任意公网域名，这样调试与查文档不受限。
6. 作为沙箱里的代码，我想在访问被黑名单拦下的域名时得到明确失败，而不是挂起，这样我能快速放弃。
7. 作为沙箱里的代码，我想在 token 被吊销后立刻失败，而不是继续用一个已失效的凭据。（v1 不做，吊销后期实现，见 Out of Scope）
8. 作为沙箱里的代码，我想看到正常的 TLS 证书，这样我不需要关掉证书校验。
9. 作为沙箱里的代码，我想在 HTTP/3 不可用时自动回落到 HTTP/2，这样浏览器类工具仍能工作。（v1 只开 HTTP/1.1：h2 下游驱动未跑通，ALPN 不宣告 h2。）
10. 作为沙箱里的代码，我想让 `git clone https://...` 正常工作，这样我能拉私有仓库。
11. 作为沙箱里的代码，我想知道出口失败的原因码，这样我能区分“没有凭据”与“域名被拒”。

### 平台与编排

12. 作为 harness，我想用 `userId` 调一次 provisioning，就拿到该用户在该沙箱上可用的幽灵 token，这样我不接触明文密钥。
13. 作为 harness，我想一次提交多个 host 的绑定，这样创建沙箱只有一次往返。
14. 作为 harness，我想在沙箱销毁时让绑定一起失效，这样没有残留凭据。
15. 作为编排平台，我想用续期控制沙箱的凭据寿命，这样停掉续期等于停掉凭据。
16. 作为编排平台，我想在 provisioning 时被拒绝并拿到明确原因（用户无此 host 的凭据），这样我能给出正确提示。
17. 作为编排平台，我想让 provisioning 只对宿主本地可见，这样不需要再造一套网络认证。

### 安全与审查

18. 作为安全审查者，我想证明真密钥从不出现在沙箱的文件系统、环境变量与进程内存里。
19. 作为安全审查者，我想证明沙箱 root 清掉 guest 内规则后仍然无法直连公网。
20. 作为安全审查者，我想证明一个沙箱的幽灵 token 在另一个沙箱里无效。
21. 作为安全审查者，我想证明幽灵 token 不能用于非绑定的 host。
22. 作为安全审查者，我想证明绑定到 `*.example.com` 这类通配后缀无法加载。
23. 作为安全审查者，我想证明 `UDP 443`（QUIC）无法绕过网关。
24. 作为安全审查者，我想证明无 SNI 的连接按 IP 判定并被拒绝。
25. 作为安全审查者，我想证明解析到私有地址的域名被拒绝，这样 DNS rebinding 不成立。
26. 作为安全审查者，我想证明网关停机时沙箱完全断网，没有直连降级。
27. 作为安全审查者，我想证明请求经 3xx 跳到别的 host 时，注入的凭据被丢弃。

### 网关运维

28. 作为网关运维，我想用一个配置文件描述黑名单、绑定与出口路由，这样策略是可审阅的。
29. 作为网关运维，我想在启动日志里看到配置哈希，这样我能确认线上跑的是哪份配置。
30. ~~作为网关运维，我想在配置加载失败时保持旧策略并记录错误，这样不会进入半新半旧状态。~~（v1 不做：策略只重启生效，没有热载，也就没有旧策略可保。启动加载失败即退出。）
31. 作为网关运维，我想在 Redis 不可达时全量拒绝，这样失效是可见的而不是静默放行。
32. 作为网关运维，我想让 TLS 终止的 CA 每个沙箱一份，这样一份泄露不影响其他沙箱。
33. 作为网关运维，我想让沙箱启动自检失败就销毁，这样不会留下“起了但没策略”的沙箱。
34. 作为网关运维，我想看到每个请求的判定结果日志行（host、决策、原因码），这样排障不需要读源码。
35. 作为网关运维，我想重启网关后从宿主恢复已注册沙箱的专属 socket 与 CA，沙箱重新建联即可，这样我不需要处理跨版本的连接状态，也不需要重建沙箱。

### 沙箱镜像与启动

36. 作为镜像维护者，我想让 CA 公钥在用户代码运行前写入信任库，这样第一条 HTTPS 请求就能成功。
37. 作为镜像维护者，我想让 `SSL_CERT_FILE`、`NODE_EXTRA_CA_CERTS`、`REQUESTS_CA_BUNDLE`、`CURL_CA_BUNDLE` 都指向同一份信任库，这样 Go、Node、Python、curl 都能工作。
38. 作为镜像维护者，我想在构建期断言 guest 内核带 TPROXY 相关配置，这样不在运行期才发现。
39. 作为镜像维护者，我想让启动自检检查 CA、mangle 规则、DNS 指向三项，这样错误在启动时暴露。
40. 作为镜像维护者，我想让 sidecar 的 ingress 控制口关闭，这样沙箱不能自己改 DNS 过滤策略。

### 用户

41. 作为用户，我想让我的 agent 能访问外部 API 而不需要我把密钥交给它，这样我的凭据安全。
42. 作为用户，我想在凭据被吊销后立刻生效，这样泄露后能止损。（v1 不做，吊销后期实现，见 Out of Scope）
43. 作为用户，我想知道我的沙箱访问了哪些域名，这样我能判断行为是否正常（审计落地前，先由网关日志承担）。

## Implementation Decisions

### 模块边界

网关是独立仓 `sandbox-gateway` 的 Rust 进程，只做三件事：监听、判定、改写。它不认识 Workspace，不依赖 agent-design 的任何代码（见 ADR 0013）。

Pingora 只提供 HTTP 引擎，不提供监听层：`listening::Service` 无法在运行期加 listener，而沙箱在运行期创建。因此网关自己写 accept loop，用 Pingora 的公开入口 `http_proxy()` + `HttpServerApp::process_new_http()`。TLS 终止用 `SslStream` + `ResumableAccept` 自建（`TlsSettings` 的构造函数是 `pub(crate)`）。

源码布局：`src/connect.rs`（UDS 监听与隧道）、`src/policy.rs`（纯判定）、`src/inject.rs`（header 改写）、`src/ca.rs`（每沙箱 CA 与叶证书）、`src/store.rs`（绑定存取）、`src/provision.rs`（注册面）、`src/config.rs`（策略配置）。

- **CONNECT 监听**（每沙箱专属 Unix Domain Socket，L1 Socket Path Binding）：每个沙箱分配独立 socket 路径（如 `/run/egress/sandboxes/<sandboxId>/connect.sock`，挂载进容器或映射自 Firecracker vsock UDS），监听器在 `Accept()` 时直接绑定 `(sandboxId, sandboxCA)`，不靠报文头或源 IP 猜身份；接受 sidecar 的 `CONNECT IP:port`，初判端口（80/443）与目标 IP 非私网后回 `200 OK` 建立字节隧道；443 端口 peek ClientHello 取 SNI 并用该沙箱专属 CA 做 TLS 终止，80 端口直接解析明文 HTTP 取 `Host`，判策略，伪造上游请求，回写。
- **DNS 监听**（UDP + TCP 53）：按同一张策略表应答或拒绝。上游解析也经它。
- **provisioning 监听**（unix socket，`0600`）：HTTP/1.1 + JSON。负责沙箱注册与凭据写入：注册 `sandboxId`（创建专属 `connect.sock`、生成每沙箱临时根 CA 并返回公钥 `caCertPEM`；无凭据沙箱也调此步拿 socket 与 CA）、建绑定与铸幽灵 token、沙箱销毁时关停并删除专属 socket、删除宿主上的 CA 文件与删绑定。

策略判定拆成 `src/policy.rs` 的纯函数：输入 `(port, host, sni, ips, method)`，输出 `Decision`。四个判定点都调它，不各写一份逻辑。

### 数据形状

```rust
// 绑定。来自 provisioning，落 Redis。
pub struct Binding {
    pub tenant: String,
    pub user: String,
    pub sandbox: String,
    pub host: String,    // 精确 host，无通配
    pub secret_id: String,
    pub header: String,
    pub prefix: String,
    pub env: String,
    pub phantom: String,
}

// 判定结果。四个判定点共用。
pub struct Decision {
    pub verdict: Verdict,        // Allow | Deny | Inject
    pub code: Option<&'static str>, // 只在拒绝时存在：EHOSTMISMATCH / EDENIED / ...
    pub binding: Option<Binding>,
}
```

幽灵 token 格式固定：`phantom_<sandboxId 前 8 位>_<128 bit 随机，32 个 hex 字符>`。

### 请求路径

1. sidecar 经本沙箱专属 Unix Domain Socket（L1 Socket Path Binding）发 `CONNECT IP:port`，带 `X-Forwarded-For`（只作日志与交叉校验，身份以监听器绑定的 `sandboxId` 为唯一权威）。网关初判端口（80/443）与目标 IP 非私网，不合规则直接关闭/RST；通过则先回 `HTTP/1.1 200 OK` 让 sidecar 开始泵送客户端字节。
2. 隧道建立后按目标端口分支：
   - **443 端口（HTTPS）**：网关 peek ClientHello 取 SNI（无 SNI 则按 IP 判定，默认拒绝 RST），用当前监听器绑定的 `sandboxCA` 签叶证书并做 TLS 终止；随后每条 HTTP 请求先校验 `Host`（`:authority`，去默认端口）必须与 TLS `SNI` 严格一致，不一致 → `EHOSTMISMATCH`（防 Domain Fronting）。
   - **80 端口（明文 HTTP）**：直接读取明文 HTTP 请求头取 `Host`（非 HTTP 报文、缺失 `Host` 或 `Host` 为字面量 IP 则默认拒绝 RST）。**80 端口严禁注入凭据**：若目标 `Host` 命中绑定表，直接拒绝（`EDENIED`），防止凭据在明文信道上暴露；未命中绑定表则转入第 7 步。
3. 443 端口用 `(sandboxId, SNI)` 查绑定。
4. 绑定存在：按请求实际是否带了绑定 header 分支。
   - 未带该 header → 不注入，原样转发（不拒绝）。匿名访问、先发无凭据请求再等 401 的客户端都走这一支。
   - 带了该 header → 一律校验，通过才替换，绝不原样透传：
     - 值为本绑定的幽灵 token → 进入第 5 步替换。
     - 值为幽灵 token，但不属于本沙箱或本 host → `EHOSTMISMATCH`。
     - 值不是幽灵 token（如沙箱自带的真值或任意串）→ `EBADPHANTOM`。
   - `Authorization: Basic`：先 base64 解码为 `user:password`，对 `user` 与 `password` 分别做上面的校验，等于幽灵 token 的字段替换为真密钥后重新编码；两个字段都不是幽灵 token → `EBADPHANTOM`。解码失败 → `EBADPHANTOM`。git、npm 等走 Basic 的客户端依赖这一条。
5. 换真密钥，替换 header，按网关自己对 `SNI/Host` 重新解析出的公网 IP 向上游拨号转发（不信任 `CONNECT` 携带的目标 IP，且解析结果命中私有地址即 `EDENIED`）。真值只在内存，不落盘、不进日志。
6. 3xx：网关自己跟。跨 host 或从 `https://` 降级到 `http://` 时丢弃注入头，重查绑定并重解析上游 IP。
7. 未绑定 host（含 80 端口未绑定域名）：剥离客户端凭据头，同样按网关自行解析并校验后的上游公网 IP 依默认策略转发。

### 权威与缓存

- 绑定与密文在 Redis，键 `tenant/sandboxId/host`，TTL 5 分钟，编排平台续期。
- 网关本地缓存解密后的值，TTL 5 分钟。缓存 miss 且 Redis 不可达 → `deny`。
- 策略（黑名单、出口路由）在配置文件。重启生效。启动日志打内容哈希。
- `userId` 只在 provisioning 用。不写进沙箱环境变量。

### CA 与沙箱身份（L1 Socket Path Binding）

- **连接来源即身份**：每沙箱在 provisioning 注册时分配专属 Unix Domain Socket（`/run/egress/sandboxes/<sandboxId>/connect.sock`）与一份专属临时根 CA（`sandboxCA`），两者直接挂在该 socket 的 listener 上下文中。无外部凭据的沙箱同样通过注册拿到专属 socket 与 CA。
- 每沙箱一份临时根，网关生成，私钥与沙箱注册信息（`sandboxId`、`tenantId`）落在宿主的沙箱目录 `/run/egress/sandboxes/<sandboxId>/`，权限仅网关进程可读，不挂载进沙箱。沙箱销毁时随专属 socket 一起删除。
- 网关重启：扫描沙箱目录，重新加载每个沙箱的 CA，重新监听其 `connect.sock`。CA 不变，沙箱信任库仍有效，sidecar 重新建联即可。
- 因为重启会重建 socket 文件，挂进沙箱的必须是 socket 所在目录，不能是 socket 文件本身（文件级 bind mount 在 inode 变化后会指向旧文件）。
- 公钥由 provisioning 返回并经 vsock 交给 guest init，在用户代码之前写入信任库，并导出四个环境变量。
- 叶证书只用当前 socket 绑定的 `sandboxCA` 签当前 host，不签通配。
- 网关用 `-tls-intercept` 关闭的 sidecar，TLS 终止发生在网关，不在 guest。

### 错误码

错误码是字符串常量，出现在日志行与 `Decision.code` 里。Rust 侧用枚举承载语义，对外只暴露码。新增：

| 码 | 何时 |
| --- | --- |
| `EHOSTMISMATCH` | 幽灵 token 与目标 host 不符，或 HTTP `Host` 与 TLS `SNI` 不一致 |
| `EBADPHANTOM` | 绑定 host 上带了绑定 header，但值（含 Basic 解码后的字段）不是幽灵 token，或 Basic 解码失败 |
| `EDENIED` | 黑名单、443 无 SNI、80 无 `Host` 或命中绑定 host、私有地址、端口不在 80/443 |
| `EBINDING` | 绑定无法加载（通配 host、无 SNI 目标、缺字段） |
| `EUPSTREAM` | 网关到上游失败 |

拒绝对外表现为 **连接被断开**，不是 HTTP 403。一律不写 HTTP 错误页。

- 握手前能判的（端口、无 SNI、黑名单、私有地址）：网关在 peek ClientHello 之后立刻关闭连接。
- 需要读 HTTP 头才能判的（`EHOSTMISMATCH`、`EBADPHANTOM`、80 端口命中绑定）：会话已交给 Pingora，由其关闭，同样是「连接关闭且无任何 HTTP 响应」。

**网关这一侧没有 TCP RST 语义**：网关与 sidecar 之间是 Unix Domain Socket，`SO_LINGER=0` 在 `AF_UNIX` 上不产生 RST。RST 出现在 sidecar → guest 的那条 TCP 上，由 sidecar 关闭连接时产生。因此验收断言「网关不写任何 HTTP 响应、且上游未被访问」；在 UDS 上断言 `ECONNRESET` 会写成永远失败的测试。

### 强制点

| 层 | 内容 |
| --- | --- |
| guest 内 | TCP（除网关 IP 豁免网段外）经 TPROXY 重定向到 sidecar；UDP/TCP 53 经 `/etc/resolv.conf` 与 `DNAT` 指向宿主网关 `:53`；其余 UDP 与 ICMP `DROP` |
| 宿主 | 每个沙箱只挂载/映射自己的网关专属 Unix Socket（L1 Socket Path Binding）；每 tap 的 DNS 只能到网关 `:53`；无公网路由 |

宿主层用 nftables 模板生成，模板随沙箱创建下发，沙箱内不暴露宿主管理面。

### 默认策略

默认放公网 + 黑名单。端口只放 80/443。`UDP 443` 丢弃，不支持 HTTP/3。v1 关闭 IPv6。

## Testing Decisions

**好的测试只测外部行为。** 测试不发 RPC 到内部函数，不 import `sandbox-gateway` 的未导出符号，不读 store 内部键名以外的结构。测试通过网关的监听面说话，断言“客户端看到什么”。

**主接缝：网关的监听面。** 起真实的每沙箱 `connect.sock`（L1 Socket Path Binding）/ DNS / provisioning unix 监听，用三个假件：

- 假 token 服务：按 `(tenant, user, host)` 返回固定密钥。
- 假上游：一个真实 HTTPS 服务，自签证书，记录收到的 header。
- 测试 CA：客户端信任池只放测试 CA，验证叶证书链。

覆盖的行为：注入成功（上游收到真值，幽灵不出现）、绑定 host 上未带 header 时原样转发且上游收不到真值、带非幽灵值时 `EBADPHANTOM`、Basic 认证解码替换（幽灵在 user 或 password 字段）、网关重启后用同一 CA 重新建联成功、host 不符（含 SNI 与 HTTP Host 不一致）、黑名单、443 无 SNI、80 端口明文放行与禁注凭据、私有地址、3xx 跨 host 与 HTTPS→HTTP 降级、端口拒绝、RST 形状、Redis 不可达、配置哈希日志、provisioning 的 `EBINDING`。

**补充接缝：`scripts/acceptance.sh`。** 新增脚本，跑全系统：

1. `UDP 443` 被丢弃。
2. guest 内 `iptables -F` 之后仍无法直连公网。
3. 网关进程停 → 全断。
4. 沙箱启动自检失败 → 沙箱销毁。

这四条只能在全系统上验，不进单元测试。

**先例。** 读写与执行用 `internal/vfs` 直测 + Linux 集成测试；远端执行用 `internal/peer` 的假对端；验收用 `cmd/agent-design/acceptance_linux_test.go` + `scripts/acceptance.sh`。网关沿用同样的三段式：纯逻辑单测、黑盒接缝测试、全系统脚本。网关侧的接缝测试是 `sandbox-gateway/tests/` 下的 Rust 集成测试，跑在生产编译的二进制上，不需要 TPROXY，因此 macOS 与 Linux 都能跑。

**不做的事。** 不为 `Decision` 函数写表驱动单测——它的输入在接缝测试里已经全部覆盖；多一个接缝就多一处可能与真实路径漂移的断言。

## Out of Scope

- 审计系统（v1 只打日志，不落库、不聚合、不查询）。
- 网关管理面（策略推送接口、吊销 RPC）。v1 靠改文件 + 重启。
- 吊销（后期实现）。v1 不保证吊销即时生效：网关本地缓存 TTL 5 分钟内，已删的 Redis 绑定仍可能被使用。
- 多租户鉴权与配额（沿用 `go-mapping.md` §8 的推迟）。
- 白名单模式、DLP、请求体扫描。
- IPv6 双栈、HTTP/3、非 80/443 端口。
- Firecracker 的编排、快照、镜像构建流水线。
- Workspace 如何映射进 Firecracker guest（virtio-fs 还是 guest 内镜像 + vsock 同步）。这一条另开 ADR。

## Further Notes

- **术语冲突**：`CONTEXT.md` 现在把 `sandbox` 列为 Workspace 的 Avoid 词。本 spec 里的“沙箱”指 Firecracker guest，与 Workspace 是两件事。落地时要在 `CONTEXT.md` 加 `Sandbox` 条目，并写清两者关系。
- **残余风险写在明面上**：默认放公网、没有补偿控制、没有审计，意味着 Workspace 内容可以被 POST 到任意域名且不留记录。ADR 0010 已记录，用户文档也要写。
- **拒绝的表现不是 403**：TPROXY 下没有应用层拦截点，验收断言“连接被重置”，断言状态码会写成永远通过的测试。
- 默认策略与第一条需求（“域名白名单过滤”）相反。以 ADR 0010 为准，不要两处并存。
