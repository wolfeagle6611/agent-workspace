# ADR 0002: 单一 runtime 入口 + 可插拔后端

Status: accepted（外部系统既成决策，本文为逆向整理）

## 背景与决策

系统有三种执行方式：完整 Linux 容器、just-bash 隔离 shell、隔离 ECMAScript 模块。三者差别极大：一个有真实网络，一个没有；一个有进程表，一个只有日志。

如果各给一套 API，调用方要学三套，agent 工具也要写三份。所以只留一个入口 `workspace.runtime.exec(source, { backend })`，由 backend id 决定 `source` 的含义：shell 命令，或模块源码。

后端抽象很窄：`connect(host)` 返回一个 Handle。同步、执行、事件全在 Handle 携带的 RPC stub 上。Workspace 不认识具体方法。

## 放弃的方案

**每后端一个方法。** `workspace.container.exec()` / `workspace.shell.exec()`。放弃原因：调用方要分支，工具层要重复实现，切换后端等于改代码。

**能力抽象（`workspace.scope()`）。** 用一个统一的权限/能力模型遮蔽后端差异。作者明确写了“没有这个抽象”，权限在构造后端时固定。放弃原因：不同后端的隔离等级不可通约，强行统一会撒谎。

## 后果

- `backend` 参数是路由，不是授权。公开网关必须自己校验允许选哪个后端。文档反复强调这一点。
- 后端 id 属于执行契约的一部分：换后端可能改变源码语言。
- 能力差异靠“注册多个后端实例”表达，例如 `codemode` 与 `codemode-readonly` 各一个。

## 对本项目的影响

Go 版照搬入口形状，但要先决定 `source` 是 `string` 还是 `[]byte`，以及 value 的类型盒子怎么表示。
