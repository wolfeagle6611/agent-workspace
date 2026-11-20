# ADR 0014: 签名通道是 Transport 的 adapter，不是转发路径里的分支

Status: accepted（本项目决策）。扩展 ADR 0013。

## 背景

有些认证方式要先知道完整请求体才能签名（例如 SigV4 要对 body 做哈希，HMAC 签名同理）。但网关的三条事实互锁：

- 上游请求头在 body 到达之前就发出（`upstream_request_filter` 在「Sending header to upstream」之前调用）。
- Pingora 能读下游 body，但没有回放 API。
- 因此「先读满 body、再发 header、再转发 body」在 `ProxyHttp` 的阶段里做不到。

把这条通道写成 `connect.rs` 里的第三个分支，会让监听、判定、转发、签名挤在一个 module 里，且每加一种签名方式就再长一个分支。

## 决策

**请求路径有两条车道，车道是 `Transport` seam 的两个 adapter，选择由凭据声明决定。**

| 车道 | adapter | 何时 |
| --- | --- | --- |
| Streaming | `egress::streaming`（`ProxyHttp` 阶段） | 头部即可决定一切 |
| Buffered | `egress::buffered`（读满 body → 插件签名 → 自建上游会话） | `Flow::needs_body()` 为真 |

- 车道在 TLS 终止后确定：查 `(sandbox, SNI)` 的绑定，问它的 `Flow` 要不要 body。请求解析之前就能决定。
- Buffered 车道复用 Pingora 的会话类型：下游 `http::server::Session`，上游 `http::v1::client::HttpSession`。它没有重写 HTTP 协议，只接管了转发顺序。
- 插件仍然不碰 IO：它只实现 `Flow::sign_request(head, cred, secret, body)`。

**Buffered 车道的边界写死**：body 上限 `policy.max_sign_body_bytes`（默认 8 MiB）；客户端用 chunked 上传即拒绝（长度未知签不出来）；上游连接不复用，请求结束即关；失败一律拒绝，不降级成 `UNSIGNED-PAYLOAD`。

## Consequences

- 加一种签名方式 = 一个 `Flow` 实现（`needs_body = true`），`egress` 与 `connect` 不动。
- `connect.rs` 只剩监听、`CONNECT` 前导、SNI、TLS 终止。
- Buffered 车道牺牲连接复用与超大请求，换取「先看全 body 再签名」的正确性。
- 与 ADR 0013 的「Pingora 只用引擎」保持兼容：Buffered 车道用的仍是 Pingora 的会话类型，只是自己驱动顺序。
