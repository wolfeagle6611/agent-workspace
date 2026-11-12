# Cloudflare Computer

一个活在 Durable Object 里的虚拟文件系统。Durable Object 持有权威状态，外部沙箱拿到的是它的投影。本文件只定义术语，不含实现细节。

## Language

### 状态与存储

**Workspace**:
一个绝对路径命名空间，根为 `/`，由 Durable Object 的 SQLite 持有。
_Avoid_: sandbox, volume, bucket

**Host store**:
Durable Object 一侧的 SQLite 库，即权威状态本身，跨 DO 复活保持。
_Avoid_: primary DB, master, backing store

**Authoritative state**:
重启后仍然成立的那份数据。只有 Host store 里的内容算数。
_Avoid_: source of truth（可读，但本文件统一用 Authoritative state）

**Mirror**:
容器一侧的副本。它服务于沙箱内的进程，不跨容器重启存活。
_Avoid_: cache, replica, snapshot

**Inode**:
VFS 内一个对象的编号。名字到 inode 的映射放在目录项里，所以改名不动内容。
_Avoid_: file id, node id

**Chunk**:
文件内容的固定长度切片，按内容哈希寻址。`CHUNK_SIZE` 为 512 KiB。
_Avoid_: block, page, part

**Manifest**:
一个文件的 chunk 哈希有序列表。它的存在让接收方跳过逐个 chunk 的探测。
_Avoid_: index, table of contents, chunk list

**Chunk store**:
负责把字节切成 chunk、按内容哈希去重、记录 `last_seen` 的 module。写事务由调用方传入，它自己不开事务。
_Avoid_: blob store（它同时管 chunk 与 blob 两层，只取一层会误导）

**Tombstone**:
一条记录“这个路径已被删除”的条目。同步靠它传播删除。
_Avoid_: delete marker, gravestone

### 同步

**Revision** (`rev`):
一次变更提交后得到的单调计数，由写入方铸造。一个 `rev` 命名一个已提交的树状态。
_Avoid_: version, sequence number, epoch, timestamp

**Watermark**:
一方已处理到哪个 `rev` 的持久记录。它让同步可以中断后续传。
_Avoid_: checkpoint（概念接近，但 checkpoint 含义太宽）, cursor（仅指带 path 的那一个）

**Push**:
把 DO 侧的新变更送到容器侧。方向上等同于 git 的 push。
_Avoid_: upload, flush, sync-out

**Pull**:
把容器侧的新变更取回 DO。它先拉条目，再按哈希补字节。
_Avoid_: fetch（`fetchChanges` / `fetchObjects` 是 pull 的内部步骤）, download, sync-in

**Coalesce**:
把同一路径的多次变更压成一条，只保留最终状态。压缩单位为路径，不是变更。
_Avoid_: compress, merge, dedupe

**Bracket**:
一次命令执行的同步包夹：push → spawn → 事件与结果 → pull。push 是安全门，失败就不执行命令。
_Avoid_: wrapper, envelope, pre/post hook

**Last-write-wins**:
冲突时后同步到的一方覆盖先到的一方，不报错。系统当前唯一生效的冲突策略。
_Avoid_: merge policy, conflict resolution（后者暗示有策略，实际没有）

**BaseRev**:
写入时携带的“我读到的版本”。与目标路径的当前 `rev` 不匹配就拒绝写入，并回传当前元数据。是可选的前置条件，不带则退回 last-write-wins。
_Avoid_: expected version, if-match（HTTP 词汇，含义窄于本系统）

### 执行面

**Runtime**:
`workspace.runtime`，唯一的执行入口。它按 backend id 把源码路由到一个后端。
_Avoid_: executor, sandbox API, shell

**Backend**:
一个执行后端的实现。它只产出 Handle，不持有执行语义。
_Avoid_: driver, provider, adapter

**Handle**:
一次 `connect()` 的产物，承载 RPC stub 与同步模式标记。
_Avoid_: connection, session, client

**Sync mode**:
Handle 声明的 `"remote"` 或 `"none"`。`"none"` 表示该后端复用 Host store，push/pull 是空操作。
_Avoid_: sync flag, storage mode

**Callable**:
后端接受结构化 `input` 并返回结构化 `value` 的能力。
_Avoid_: JSON mode, structured mode

**Execution**:
一次 `exec()` 产生的运行实例，有 id、事件流与最终结果。
_Avoid_: run, job, task, invocation

**Interrupt**:
执行在等外部答复时释放自己，收到答复后重放到该调用点并继续。
_Avoid_: pause, suspend, await

### 生命周期与部署

**Incarnation**:
DO 的一个实例化周期。内存状态在周期之间清空，`ctx.storage` 留下。
_Avoid_: boot, generation, lifetime

**Hibernation**:
空闲的 DO 被回收，但可休眠的 WebSocket 与其附件保留。
_Avoid_: sleep, suspend, pause

**computerd**:
容器内的守护进程。它提供 FUSE 挂载、HTTP/WebSocket 服务与命令执行。
_Avoid_: agent, sidecar, daemon（太泛）

**Reverse dial**:
容器主动连回 DO 的 `/connect`，使 WebSocket 由 DO 接受。方向反转是为了让出网流量经过 DO 的路由。
_Avoid_: callback, tunnel, port forward

**Reserved path**:
系统自己使用的路径，当前只有 `/.computer`（输出全文）。它会被列出，但默认不进备份、不进 `git status`、不允许作为挂载点。
_Avoid_: hidden dir, dotfile（两者都不表达“系统所有”）

**Volume**:
挂载点上暴露的文件操作层。它持有打开的句柄与每个句柄的写缓冲，把改动提交给权威库。
_Avoid_: mount driver, FUSE layer（后者只是它的一个 adapter）

**Mount point**:
每个工作区的一个目录，子进程经它看到 VFS。它的生命周期与工作区绑定，残留的僵尸挂载在 `Open` 时清理。
_Avoid_: mount dir, sandbox root（后者指工作区路径根，不是挂载点）

**FUSE mount**:
容器内看到的 Workspace 视图。读写经它落到容器侧 VFS，再经同步回到 DO。
_Avoid_: disk, volume mount, bind mount

**Isolate**:
Dynamic Worker 里的一段隔离 JavaScript。它没有真实网络，也没有真实文件系统。
_Avoid_: sandbox（与容器混用会失真）, VM, microVM

### 容易混淆的词

**Stub**:
本系统有两个不同的东西都叫 stub。其一是 RPC 代理对象；其二是 lazy mount 里“占位但未取字节”的文件节点。
使用该词时必须限定为 `RPC stub` 或 `mount stub`。
_Avoid_: 裸用 stub

**Mount**:
两件事共用一个词：把外部来源铺进 Workspace 的只读子树（R2、Worker bundle），以及容器内把 Workspace 挂到 `/workspace` 的 FUSE 挂载。
前者叫 `data mount`，后者叫 `FUSE mount`。
_Avoid_: 裸用 mount

**Native root**:
容器自己磁盘上的目录，绕过 VFS。`MOUNT_IGNORE` 命中时才会出现。
_Avoid_: local path, host path, passthrough
