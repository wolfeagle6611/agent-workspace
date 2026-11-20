# ADR 0016: 出口网关的身份与信任模型

Status: accepted（本项目决策）

## 背景

对齐 `cloudflare/computer@9d11fa7` 时，身份与信任三处都不同：

| 维度 | 上游 | 我们 |
| --- | --- | --- |
| 身份来源 | hop 凭证 `x-workspace-egress-token`，每次启动一个随机 UUID（`backends/container/container-backend.ts:289`），在共享的 `/api` 端点校验（`:496-497`） | 哪个 Unix Domain Socket `Accept()` 到这条连接。每沙箱一个 socket，`0600`，目录 `0700` |
| CA | 平台一份 intercepting CA，所有容器共用（`docs/20_remote_exec.md:311`） | 每沙箱一份临时根 CA（`src/ca.rs:38-88`），`ca.key` `0600` |
| 容器复用 | 一个容器可被多个调用方复用的，按命令传 egress，于是需要 `egressKey` / 重路由 / 拒绝 / 独占四件套（`remote-exec/sandbox.ts:300-345`） | 一沙箱一身份，注册时钉住；`bind` 不接受请求体里的身份 |

三条是同一个模型的三面，因此记在一个 ADR 里，不拆成三份互相引用的半篇。

## 决策

**沙箱是唯一身份，网关不持有任何逐跳凭证；CA 每沙箱一份；一个沙箱只属于一个 `(tenantId, userId)`。**

1. **不加 hop 凭证。** 身份等于「能打开该 `connect.sock`」。上游 token 解决的是「一个共享端点上有多个容器」这个形状，我们没有共享端点。guest 内代码若能打开 socket，它与 sidecar 同属一个不可信主体，网关对二者的授权本来就相同 —— 加一个 guest 可读到的凭证，不产生新边界。
2. **CA 每沙箱一份。** 一份泄露的爆炸半径不同。代价是多几个小文件与现场签叶证书，`leaf_cache` 已把签名开销压到每 host 一次。
3. **一沙箱一身份，写成显式约束。** 重复注册同一 `sandboxId` 且 `(tenantId, userId)` 不同时返回 `409 EIDENTITY`，不静默沿用、不重建沙箱。换身份只能换沙箱。

## 已知残留风险

宿主上以 root 运行、或被授予该沙箱目录读权限的进程，可以冒充该沙箱。这与上游同层风险等价：上游 token 存在后端进程内存里，能读该进程内存的一方同样可以冒充。这条风险写进 spec，不再用额外机制补偿。

## Considered Options

- **照抄 hop token**。否决：换不来新边界，还多一个 guest 可能读到的凭证。
- **CA 改为宿主一份**。否决：省几个文件，换一份泄露影响全宿主。
- **允许一沙箱多身份（跟上游的多调用方复用）**。否决：那套重路由补偿是「一容器一条路由」的补丁，我们采纳它要拆掉 Socket Path Binding 的前提。代价是「换身份要换沙箱」，这条更硬，但边界更简单。
- **`SO_PEERCRED` 校 `connect.sock` 的对端**。否决：socket 经 vsock 映射时对端是 VMM 的单一 uid，无法区分沙箱。

## Consequences

- provisioning 的注册体里 `tenantId` / `userId` 一旦写入不可改；重复注册只做幂等。
- 一个沙箱只能有一套绑定身份，绑定表按 `(sandbox, host)` 索引，`tenant` / `user` 由注册时继承。
- 对齐文档 `docs/architecture/egress-gateway-alignment.md` 第 12、13、14、15 项依赖本 ADR。
