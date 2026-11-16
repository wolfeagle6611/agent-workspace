# ADR 0013: 出口网关用 Rust + Pingora 实现

Status: accepted（本项目决策）

## 背景

spec `docs/specs/egress-gateway.md` 按 Go 写：新增 `internal/egress`、沿用 `go-mapping.md §7` 的哨兵错误与 `errors.As`、沿用 Go 先例的三段式测试。Pingora 是 Rust 代理框架。语言与 spec 的前提冲突，必须选一边。

## 决策

**网关用 Rust 写成独立进程，放在独立仓 `sandbox-gateway`，作为沙箱宿主上的进程运行。** agent-design 保持 Go，只经 provisioning 的 Unix Socket 与网关对话，不 import 网关代码。

**Pingora 只用引擎，不用它的监听层。** 沙箱在运行期创建，每个沙箱一个专属 Unix Domain Socket。Pingora 的 `listening::Service` 只能在 `run_forever()` 前加 listener，不满足这一点。因此网关自己写 accept loop，用 Pingora 提供的公开入口 `http_proxy()` + `HttpServerApp::process_new_http()`。Pingora 负责：HTTP/1 与 HTTP/2 解析、上游连接、`ProxyHttp` 阶段。网关负责：UDS 监听、`CONNECT` 前导、TLS 终止。

**TLS 后端用 boringssl。** 证书按 SNI 现场签发，只有 openssl 与 boringssl 提供 `certificate_callback`。选 boringssl 是为了静态链接、不受宿主 openssl 版本影响。

**spec 的语义不变，实现载体变。** wire 协议、判定规则、错误码、强制点、默认策略一律照 spec 执行。作废的只有 Go 专属内容：模块路径、哨兵错误、Go 测试先例。

## Considered Options

- **放弃 Pingora，全用 tokio + hyper**：可控性最高，但要自己实现 HTTP/2、上游连接复用与超时，与 spec 的「只做三件事」不符。
- **每沙箱起一个 Pingora `Server`**：全用内置 API，但每沙箱一套 worker 线程池，几百沙箱不可接受。
- **改 sidecar 协议按端口连不同 socket**：`cloudflare/proxy-everything` 是第三方，不能改。

## Consequences

- **失去** Pingora 内置监听的优雅重启（fd 传递）、连接过滤器、worker 间连接分担。spec 的重启方案本来就是「扫描沙箱目录重新监听」，不受影响。
- **失去** Go 生态的错误模型。`Decision` 的 `Code` 是字符串，`errors.Is` 换成枚举匹配。对外仍是 TCP RST。
- **重复一件小事**：TLS 握手用 Pingora 的 `SslStream` + `ResumableAccept`，但 `SslAcceptor` 与证书回调由网关自建（`TlsSettings` 的构造函数是 `pub(crate)`）。
