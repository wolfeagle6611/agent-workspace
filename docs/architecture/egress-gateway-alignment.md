# 出口网关能力对齐：cloudflare/computer

> 基线：`cloudflare/computer@9d11fa7`（v0.11.3，2026-11-17）。行号都是该 commit 的行号。
> 对照对象：本仓 `sandbox-gateway`（spec：`docs/specs/egress-gateway.md`）。
> 上游架构梳理见 `computer.md`。本文只做取舍，不复述上游机制。

## 一句话结论

上游的出口网关是**一个交给调用方的拦截点**；我们的出口网关是**一个自己持有判定权的强制点**。六层对照下来只有一个能力真缺（按沙箱断网），三项需要写进 spec 的显式约束，其余是「已具备」或「模型里没有」。

| 结论 | 数量 |
| --- | --- |
| `adopt`（搬进来） | 3 |
| `already-have`（已有，不搬） | 4 |
| `not-following`（看过，不做，附理由） | 9 |
| `N/A`（前提不存在） | 1 |

## 对照表

结论词义：`adopt` = 采纳上游能力；`already-have` = 我们已有等价或更强的机制；`not-following` = 上游有，我们决定不做；`N/A` = 上游那层的前提在我们这里不存在。

| # | 层 | 能力 | 上游事实（`9d11fa7`） | 我们现状 | 结论 | 理由 |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | ① 策略 | 按沙箱断网 | `runtime/egress.ts:5` 的 `{ mode: "none" }` | 缺。只能靠黑名单穷举 | **`adopt`** | 离线构建、纯计算沙箱是真需求 |
| 2 | ① 策略 | 完整 per-sandbox 策略（每沙箱一份黑名单） | 无对应物（上游策略是每实例一个枚举） | 缺 | `not-following` | 策略会从「一份可审阅的配置文件」变成「运行期数据」，与 spec 用户故事 28 冲突 |
| 3 | ① 策略 | `direct` 具名档 | `runtime/egress.ts:6` | 现在就是（`policy.blacklist = []` + `allow_ports`） | `already-have` | 具名不增加语义 |
| 4 | ① 策略 | `http-gateway` 具名档：判定权交给调用方 `Fetcher` | `runtime/egress.ts:7`、`hostEgressFetch` `:26-36` | 无，且**不应有** | `not-following` | 见 ADR 0015 |
| 5 | ① 策略 | 策略进模型上下文（按档位给三句话） | `backends/container/container-backend.ts:189-193` | 无 | `not-following` | 网关不知道模型存在。归属 `agent-design` 的 harness |
| 6 | ② 凭证 | hop 凭证 `x-workspace-egress-token` | `runtime/egress.ts:1`；生成 `container-backend.ts:289`（`crypto.randomUUID()`）；校验 `:496-497` | 身份来自「哪个 socket `Accept()` 到」（`src/connect.rs:74-110`，socket `0600`、目录 `0700`） | `already-have` | 上游 token 解决「共享 `/api` 端点上有多个容器」。我们没有共享端点 |
| 7 | ② 凭证 | 原始 URL 透传 `x-workspace-egress-url` | `runtime/egress.ts:2`；读取 `container-backend.ts:500`；转发前删除 `:510-511` | `CONNECT` 目标 + SNI，且 `Host` 与 SNI 必须一致（spec「请求路径」第 2 步） | `already-have` | 溯源信息已覆盖 |
| 8 | ③ 信任 | 等 CA 出现（100 × 0.1s） | `remote-exec/egress.ts:34-38` | 无 | `not-following` | `provisioning` 同步返回 `caCertPEM`（`sandbox-gateway` `src/provision/mod.rs:478`），沙箱启动前 CA 已存在 |
| 9 | ③ 信任 | 幂等去重（`begin`/`end` 注释块） | `remote-exec/egress.ts:39-50` | 无 | `not-following` | 同一沙箱的 CA 跨重启不变（`src/ca.rs:38`），重复写入无害 |
| 10 | ③ 信任 | 写发行版 anchors 目录 | `remote-exec/egress.ts:51-53` | spec 只写「写入信任库」 | **`adopt`** | 镜像里后续 `apt-get` / `pip` 可能重跑 `update-ca-certificates`，把注入的 CA 冲掉 |
| 11 | ③ 信任 | 四个环境变量 + 统一信任库 | `remote-exec/egress.ts:18-23`（Node 指单 CA，其余指 bundle） | spec 已写四个都指同一份信任库 | `already-have` | 上游分开是因为它的 bundle 是拼接产物，不是语义差异 |
| 12 | ③ 信任 | CA 归属：平台共享一份 | `docs/20_remote_exec.md:311`「the platform's intercepting CA」 | 每沙箱一份临时根 CA（`src/ca.rs:38-88`，`ca.key` `0600`） | `not-following`（不跟共享 CA） | 一份泄露的爆炸半径不同。见 ADR 0016 |
| 13 | ④ 路由 | 一容器多调用方复用（`withEgress` 按命令传 egress） | `remote-exec/sandbox.ts:133`、`:140`；`docs/20_remote_exec.md:343-354` | 一沙箱一身份，注册时钉住；`bind` 不接受请求体里的身份 | `not-following` | 采纳它要拆掉 Socket Path Binding 的前提。见 ADR 0016 |
| 14 | ④ 路由 | 重路由补偿四件套（`egressKey` 共用 / 重路由 / 有人用则拒绝 / stub 无 key 独占） | `remote-exec/sandbox.ts:178`、`:300-345` | 无，且不需要 | `not-following` | 四条都是「一容器一条路由」的补丁。我们的模型里没有这个失败模式。代价：**换身份只能换沙箱** |
| 15 | ④ 路由 | 「一 Sandbox 一身份」写成显式约束 | 无对应物（上游允许多调用方） | 有事实，无约束：重复注册换了 `(tenant, user)` 会静默沿用旧身份（`src/provision/mod.rs:150-152`） | **`adopt`** | 静默沿用会把绑定记到错误租户名下而不报错 |
| 16 | ⑤ isolate | `globalOutbound`：`null` / 直连 / `gateway` | `runtime/egress.ts:9-20`；codemode `:1827` | 本仓不含 isolate 执行面 | `N/A` | 模型里没有这一层。`not-following` 会误导读者以为我们评估过并拒绝 |
| 17 | ⑥ 拦截 API | 进程内 `interceptOutboundHttp` / `interceptAllOutboundHttp` / `interceptOutboundHttps` | `backends/container/container-host.ts:89-90`、`:328-355` | 无。对应物是宿主 nftables + 每沙箱 UDS | `not-following` | 上游的 API 成立是因为拦截点属于平台运行时。做等价 API 等于在网关里重造 sidecar |

## 三处 `adopt` 的共同点

三条都不是「学上游」，是「补我们自己缺的」：

1. **按沙箱断网**：现在做不到，只能黑名单穷举。
2. **anchors 目录**：spec 少写了一句，镜像更新证书会冲掉注入的 CA。
3. **一沙箱一身份**：事实存在但没强制，重复注册会静默记错租户。

## 上游这台网关没有的东西

写在这里，防止读者以为对照表漏了什么：

| 没有 | 证据 |
| --- | --- |
| 按 host 的允许 / 拒绝规则 | `grep -rn "blacklist\|allowlist\|allowList" packages/computer/src` 为空 |
| 凭据注入（幽灵 token 换真密钥） | 全树 `grep -rli "phantom"` 无相关命中 |
| 策略配置文件 | 仓库根只有 `package.json` / `package-lock.json`；策略是构造参数 |
| DNS 拦截 | 无 `:53` 监听 |
| 独立进程形态 | 网关是 Worker / `Fetcher`，跑在平台上，与容器同属一次部署 |

## 复核方法

```bash
# 取上游该 commit 的全树
gh api repos/cloudflare/computer/tarball/9d11fa7 | tar xz

# 六层的入口
cat  packages/computer/src/runtime/egress.ts
grep -n "egressToken\|WORKSPACE_EGRESS" packages/computer/src/backends/container/container-backend.ts
cat  packages/computer/src/remote-exec/egress.ts
grep -n "egressKey\|type Route\|#route" packages/computer/src/remote-exec/sandbox.ts
grep -rn "interceptOutbound" packages/computer/src/backends/container/container-host.ts

# 我们这一侧
cd ../sandbox-gateway
grep -n "fn decide\|enum Verdict" src/policy.rs
grep -n "load_or_create\|ca_cert_pem" src/ca.rs
grep -n "caCertPEM\|register_with_identity" src/provision/mod.rs
```

漂移检查：`gh api "repos/cloudflare/computer/compare/9d11fa7...main" --jq '.ahead_by, (.files[]? | .filename)'`。2026-11-20 复核时 `main` 只前进 5 个 commit，与本主题相关的只有 `examples/egress/Dockerfile`。

## 相关文档

- 行为规格：`docs/specs/egress-gateway.md`
- 上游机制：`docs/architecture/computer.md`（注意：其 §8.1 只写了两行策略表，漏了 `runtime/egress.ts`）
- 决策：ADR 0010（出口网关）、0011（幽灵 token）、0013（Rust + Pingora）、0015（判定权留在网关内）、0016（身份与信任模型）
