# 可复用评级与 Go 映射

> 目标：在 Go 里做同构系统——权威存储 + 可插拔执行后端。
> 评级只对机制，不对代码。每行结论只能是 **照搬 / 改造 / 不要照搬**。

> 阶段划分见 §2 与 ADR 0006：第一阶段只有 `sync: "none"` 的本地后端。

## 1. 评级表

| 机制 | CF 依赖 | 换到 Go 的代价 | 收益 | 结论 |
| --- | --- | --- | --- | --- |
| inode + dirent + 内容寻址 chunk 的 schema | 无（纯 SQLite） | 低。DDL 直译，索引照建 | 改名 O(1)，硬链接免费，去重白拿 | **照搬** |
| `rev` 单调 + 水位表与数据同事务 | 无 | 低。一条 `UPDATE` 与写入同一事务 | 崩溃可续，且水位不会与数据漂 | **照搬** |
| `(rev, path)` 双坐标游标 | 无 | 中。需要在 coalesce 时保证 `rev` 升序、`path` 升序 | 大改名中断只重放一批，不重放整个 rev | **照搬** |
| 按路径合并（coalesce） | 无 | 低。一个 map + 稳定排序 | 线上条目数与编辑次数解耦 | **照搬** |
| 固定 512 KiB 分块 + sha256 | 无 | 低 | 实现简单，寻址可预测 | **改造**：按负载选块大小；若有大文件插入型写入，换内容定义分块 |
| 先探测再传字节（`hasObjects` / `fetchObjects`） | 无 | 低 | 往返固定 2 次，不随文件数增长 | **照搬** |
| `senderRev` 双分支 | 无 | 中。容易漏掉 `= 0` 分支 | 外部写入不会被接收方吞掉 | **照搬** |
| 单一 `runtime.exec` + 窄 Backend / Handle | 无 | 低 | 加后端不改调用方 | **照搬** |
| `sync: "remote" \| "none"` 模式标记 | 无 | 低 | 本地后端零同步，无需特殊分支 | **照搬** |
| 后端权限在构造时固定 | 无 | 低。一个结构体字段 | 权限收不回来这件事被写进类型 | **照搬** |
| 输出截断 + 落盘全文 + 尾部窗口 | 无 | 低 | 上下文有界，全文仍可读 | **照搬** |
| 反向拨号 + capnweb | `ctx.acceptWebSocket`、`interceptOutboundHttp` | 高。Go 侧要自己写线协议与握手 | Cloudflare 平台上换来休眠与出网统一出口 | **改造**：本机执行体可正向连；远端执行体保留反向拨号 |
| DO 输入门串行化写入 | 平台运行时 | 中。Go 要自己给每个工作区一个写协程或 `sync.Mutex` | 免掉同实例写-写冲突 | **改造** |
| 休眠与化身模型 | 平台运行时 | 高。Go 没有等价的自动驱逐 | 休眠让空闲工作区不占内存 | **不要照搬**：改成显式 `Close()` 或空闲回收 |
| FUSE 挂载 | 无（Linux / macFUSE 能力） | 高。go-fuse 驱动 + 挂载点生命周期 | 子进程看到真路径，写入即时生效 | **改造**：第一阶段就做，同进程直接服务权威库 |
| last-write-wins + 单写者约束 | 无 | 低 | 无冲突解决代码 | **照搬**：并把三条用法约束写进文档，否则用户会丢数据 |
| 宿主模块协议（`callId` 幂等键 + `interrupt` 重放） | 无 | 中。重放语义要真的实现 | 执行可以等人几小时不占资源 | **改造**：先只做 `callId`，`interrupt` 后置 |

## 2. 分层结论

| 层 | 第一阶段 | 第二阶段（出现第二个存储时） |
| --- | --- | --- |
| L1 存储 | 全部落地 | — |
| L2 协议 | 只保留 `rev` 铸造 | 水位、合并、双坐标游标、探测补字节 |
| L3 执行 | 落地，含 `sync: "none"` 路径 | 同一接口，加 remote 实现 |
| L4 传输 | 只做 FUSE | WebSocket / gRPC / 反向拨号 |


按层看更清楚：

**阶段说明。** L2 的结论不变，但它只在第二个执行后端出现时才落地。第一阶段的本地后端声明 `sync: "none"`，FUSE 由同进程服务并直接读写权威库，没有第二份存储，也就没有对手方。见 ADR 0006。

**L2 是真正值钱的部分，但只在需要第二个存储时才值钱。** 它与云平台无关，换个语言照用。L3 是接口品味，值得学形状。L4 里 FUSE 第一阶段就要做，传输层可以换。

## 3. Go 接口草图

> 示意代码，不可编译。写出来只为固定形状与责任边界。

### 3.1 后端与句柄

```go
// Backend 只产出句柄。它不认识文件语义，也不认识同步细节。
type Backend interface {
	ID() string
	Kind() string                      // 诊断用："container" / "shell" / "module"
	Callable() bool                    // 是否收结构化 Input
	Connect(ctx context.Context, host Host) (Handle, error)
}

// Handle 是一次连接的产物。
type Handle interface {
	Sync() SyncMode                    // SyncRemote | SyncNone
	RuntimeID() string                 // 执行体进程身份，用于拒绝过期句柄
	Closed() <-chan struct{}           // 传输断开时关闭
	Close() error
	// 具体能力在这里按需暴露成接口，而不是方法堆在 Handle 上
	SyncClient() SyncClient            // nil 当 SyncNone
	ExecClient() ExecClient
}

type SyncMode int

const (
	SyncRemote SyncMode = iota // 有独立存储，需要 push/pull
	SyncNone                   // 复用权威存储，push/pull 是空操作
)
```

### 3.2 执行入口

```go
type Runtime struct {
	backends map[string]Backend
	order    []string
}

func (r *Runtime) Exec(ctx context.Context, source string, opts ExecOptions) (*Exec, error)

type ExecOptions struct {
	ID       string            // 供 getExec / killExec 再挂接
	Backend  string            // 路由，不是授权
	Cwd      string            // 绝对路径
	Env      map[string]string
	Stdin    io.Reader
	Input    any               // 仅 callable 后端
	Metadata map[string]string // 只给宿主模块
	Timeout  time.Duration
}

// Exec 既是事件流，也是结果来源。单消费者。
type Exec struct {
	ID      string
	Backend string
	Events  <-chan Event
	Result  func(context.Context) (Result, error)
	Kill    func(context.Context, os.Signal) error
}

type Result struct {
	Status   Status          // Completed | Failed | Cancelled
	ExitCode int
	Output   execout.Result  // 两个流的尾部、全文路径、总量、截断标记
	Value    any             // 仅模块后端
	Pushed   int
	Pulled   int
	Sync     SyncOutcome     // Complete | Pending | Lost
}
```

### 3.3 变更与水位

```go
// 线上只传条目与哈希，不内联字节。
type Change struct {
	Rev    int64
	Path   string
	Kind   ChangeKind        // File | Dir | Symlink | Delete
	Mode   uint32
	MTime  time.Time
	Chunks []ChunkRef        // 仅 File
	Prior  *NodeMeta         // 仅 Delete，可选
}

type ChunkRef struct {
	Hash [32]byte
	Size int64
}

// 水位与它描述的数据在同一事务写入。
type Watermark struct {
	PushRev  int64 // 已推送到对端的最后一个 rev
	FetchRev int64 // 已从对端拉到的 rev
	FetchPath string // nil 语义用空串；表示该 rev 内推进到哪个路径
}
```

### 3.4 同步接口（第二阶段）

> 第一阶段不实现。形状先定，保证第二个后端出现时不改调用方。

```go
type SyncClient interface {
	Push(ctx context.Context, senderRev int64, changes <-chan Change) (PushResult, error)
	FetchChanges(ctx context.Context, after Watermark) (ChangeStream, error)
	HasObjects(ctx context.Context, hashes [][32]byte) ([][32]byte, error)
	FetchObjects(ctx context.Context, hashes [][32]byte) (ObjectStream, error)
	PushObjects(ctx context.Context, objects ObjectStream) error
}

// 两种失败必须能被调用方区分，所以用哨兵错误而不是字符串。
var (
	ErrExecLost = errors.New("execution lost: runtime replaced")  // 对应 EEXEC_LOST
	ErrSyncLost = errors.New("sync lost: runtime replaced")       // 对应 ESYNC_LOST
)
```

### 3.5 编排顺序（伪代码）

```go
func (w *Workspace) execContainer(ctx context.Context, cmd string, opts ExecOptions) (*Exec, error) {
	if err := w.push(ctx); err != nil {      // 安全门：失败不 spawn
		return nil, err
	}
	ex, err := w.backend.Exec(ctx, cmd, opts)
	if err != nil {
		return nil, err
	}
	go func() {
		<-ex.Done()
		w.markPullPending(ex.RuntimeID())     // pull 失败不回滚命令结果
	}()
	return ex, nil
}
```

## 4. 不要照搬的三点及理由

| 点 | 为什么不照搬 | 替代 |
| --- | --- | --- |
| 休眠 / 化身自动驱逐 | 依赖平台把空闲 isolate 回收。Go 进程不会自己消失 | 显式 `Close()`、引用计数、空闲超时回收 |
| capnweb 线格式 | 为 Workers RPC 的值语义设计（`ReadableStream` 一等公民）。Go 侧没有这个运行时 | gRPC 流或自定帧 + `io.Reader` |
| 平台托管密钥 | 上游由容器平台注入，Go 侧没有注入点 | 自己生成、自己轮换（见主文档 §8.4） |

## 5. 落地顺序建议

第一阶段只有本地后端，所以顺序变了：先让读写和执行真正跑通，同步协议推迟。

| # | 做什么 | 验证 |
| --- | --- | --- |
| 1 | schema + inode 解析 + `readFile` / `writeFile` + `rev` 铸造 | 写进去再读出来；重启进程后仍在；`rev` 严格递增 |
| 2 | FUSE 挂载（go-fuse）+ 挂载点生命周期 + 残留卸载 | `ls` / `echo >` / `rm` 从挂载点看出与 `fs` 一致；崩溃后 `Open` 能清残留 |
| 3 | 单一 `Exec` 入口 + 子进程后端（`sync: "none"`） | 换后端不改调用方；子进程写的文件立刻出现在 `fs` 里 |
| 4 | 取消与超时（进程组）+ 输出截断与落盘 | 超时后无孤儿进程；打印 10 万行不爆内存 |
| 5 | `baseRev` 前置条件 | 两个并发读-改-写，第二个拿到结构化冲突，不静默丢写 |
| 6 | （第二阶段）远端 / 容器后端 + push / pull + 水位 | 杀掉对端后重连，续传不重复应用 |

第 1–2 步之前不要碰执行面。第 6 步之前不要写水位表：没有对手方时，那些表只会腐烂。

## 6. 第一阶段已定约定

这些决定不各自成 ADR（易反转，或只是实现细节），但落地时必须一致。

| 主题 | 决定 | 理由 |
| --- | --- | --- |
| FUSE 写入 | 内存缓冲；满 512 KiB、或 `flush` / `release` 时入库 | 碎片写不放大；崩溃至多丢一个缓冲块 |
| 缓冲与持久性 | `fsync` 与 `release` 必须等入库完成后返回 | 否则 `echo > file` 之后 `fs` 读到旧内容 |
| chunk 存储 | 独立 module，只暴露 `Put` / `Chunks` / `Bytes` / `Counts`；写事务由调用方传入 | FUSE 与导入将来复用同一 interface；去重规则只有一处 |
| exec 与 `baseRev` | exec 写入总是生效，不受前置条件约束（ADR 0007） | 子进程无法表达前置条件；事后报冲突只会变成噪音 |
| SQLite 并发 | 单写协程；读走 WAL 连接池 | 显式化“输入门串行”，避免 `SQLITE_BUSY` 随机失败 |
| 并发执行 | 每工作区默认 1，可配置 | 让单写者约束结构性成立，而不是靠文档提醒 |
| 挂载点 | `/run/ws/<id>`；`Open` 原子创建并惰性卸载残留；`Close` 卸载后删除 | 僵尸挂载是这类系统最常见的运维事故 |
| FUSE 适配层 | go-fuse 只做 syscall → `Volume` 的翻译；entry / attr / negative 超时全为 0 | 规则都留在可测的 `Volume` 里；开缓存会让挂载点与库不一致 |
| 读路径 | `ReadAt` 只读命中的 chunk；`Object` 按哈希取一片 | FUSE 的 `read()` 不把整文件读进内存 |
| 测试 | FUSE 集成测试仅 Linux CI；单元测试直接打 `fs` 层 | 不维护两套执行语义 |
| 模块后端 | 第一阶段不做，只定接口 | 最容易后加，且加时不碰主干 |
| `baseRev` 目录语义 | `mkdir` 查父目录；`rm` 查目标节点，`baseRev=0` 表示“期望不存在”；`rename` 查源节点，目标已存在时另要 `TargetBaseRev` | 只有 `rename` 会覆盖，只有它需要两个前置条件 |
| FUSE 缓存 | `nodefs` 节点树 + 属性 TTL = 0，先保证 `fs` 与挂载点永远一致 | 没数据证明 `stat` 是瓶颈前，不承担失效 bug 的风险 |
| 执行记录 | 落库 `exec_id / 命令 / 起止时间 / 状态`；重启后查旧 id 返回 `ErrExecLost`，不是“未知 id” | 区分“丢失”与“不存在”是两种事实 |
| 保留路径 | `/.computer` 是系统目录：`readdir("/")` 仍列出它，但备份、`git status`、变更报告默认忽略；不允许在此建立挂载点 | 打破“根下不自动创建”的例外必须显式登记 |
| 挂载时机 | `mount.Session` 独占挂载协议：`Mount` 惰性且单飞，`Unmount` 顺序为冲缓冲 → 卸载 → 删目录；`Workspace.Close` 先取消执行，等上限，再关库 | 顺序写错就丢缓冲；协议住在挂载包里，调用方无处写错 |
| 挂载点命名 | `<挂载根>/<数据库文件名去扩展名>-<绝对路径哈希前 8 位>` | `Prepare` 会清掉挂载点上的占用者，同名数据库共用挂载点会卸载活工作区并串库 |
| exec 可见性 | 子进程 `close()` 后可见；未关的写入最多 512 KiB 不可见 | 沿用 FUSE 缓冲规则，不为此改 `Volume` |
| exec 命令形状 | `/bin/sh -c`；环境 = 进程环境加 `ExecOptions.Env`；stdin 为空设备 | 上游 `source` 是 shell 命令；继承环境让 `git` / `go` 可用；读 stdin 不会挂死 |
| exec 取消 | 进程组 SIGTERM，宽限 5 秒，再 SIGKILL；信号结束记 `Status Cancelled` 与 `ExitCode -1`，并带回 `ctx.Err()` | `-1` 保留真实死因；`ctx.Err()` 是调用方区分超时与取消的唯一手段 |
| exec 超时 | `ExecOptions.Timeout`；从 `Connect` 与 `Resolve` 之后起算 | 首次挂载慢不能伪装成命令超时 |
| 结果形状 | `Result{Status, ExitCode, Output}`；每个流带 `Tail / Path / Bytes / Lines / Truncated` | 输出是一件事；摊成十二个字段每个后端都要填对 |
| 尾部窗口 | 字节环，`Finalize` 时再裁到最后 N 行；默认 64 KiB / 2000 行，经 `execout.Limits` 传入 | 字节上限天然成立；行裁剪只发生一次，不进热路径；限额可按后端或配置覆盖 |
| 输出全文创建 | `Open` 无条件 `MkdirAll("/.computer/output")`，失败即 `Open` 失败 | 列目录在有无命令时一致；半开工作区会让冲突语义变模糊 |
| 输出写入路径 | 直连 `vfs.Store`，不经 `Volume`；不受 `baseRev`；`rev` 照铸 | 挂载失败也要能落盘；加例外会让第 5 步与第二阶段都要记住它 |
| 输出裁剪 | 全文放工作区 `/.computer/output/<backend>.<id>.stdout|stderr.log`，路径写进结果；exec 结束时按 `rev` 降序保留最近 50 个条目 | 与上游路径一致，读取复用现有 `fs` API；目录即契约，名字模式不参与裁剪；裁剪失败不改写结果 |
| 执行与 `Close` | `Close` 取消工作区根 ctx，最多等 5 秒，再关库 | 库不在活写者下面关闭 |
| 目录创建与 `rev` | `mkdir` 与建目录不推进 `rev`（既有行为） | 第二阶段做变更流时要知道：目录创建不是变更事件 |
| backend 路由 | `ExecOptions.Backend` 空串 = 唯一注册的后端；多个后端加空串 = `EINVAL`；未注册 id = `ENOBACKEND` | 空串猜错后端比报错更糟 |
| 执行句柄 | 每次 `Exec` 一次 `Connect` / `Close`；`Result` 可重复调，只等一次 | 子进程后端连接无成本；缓存是远端传输的需要 |

## 7. 错误模型

一条规则：**哨兵错误供 `errors.Is`，结构化类型供 `errors.As`。** 码值沿用上游的字符串码，便于跨语言读日志。上游没有 `ECONFLICT`，因为它没有 `baseRev`；这一条是本项目新增。

| 码 | 何时 | 附带字段 |
| --- | --- | --- |
| `ENOENT` | 路径不存在 | `Path` |
| `EEXIST` | `rename` 目标已存在且未给 `TargetBaseRev` | `Path`, `TargetPath` |
| `ENOTDIR` | 路径中间段是文件 | `Path` |
| `EISDIR` | 对目录做文件操作 | `Path` |
| `ENOTEMPTY` | `rm` 目录非空且未递归 | `Path` |
| `EROFS` | 写保留路径或只读区 | `Path` |
| `EINVAL` | 相对路径、含 `..` 越界、路径非绝对 | `Path` |
| `ECONFLICT` | `baseRev` 与当前版本不匹配 | `Path`, `BaseRev`, `CurrentRev` |
| `ENOBACKEND` | `ExecOptions.Backend` 未注册 | `Path`（该 id） |
| `EEXEC_LOST` | 执行 id 属于已消失的进程 | `ExecID` |
| `ESYNC_LOST` | 第二阶段：同步对端被替换 | `RuntimeIDs` |

```go
// 示意代码，不可编译。
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("version conflict")
	ErrExecLost = errors.New("execution lost")
)

type Error struct {
	Code       string   // ENOENT / ECONFLICT / ...
	Path       string
	TargetPath string   // 仅 rename
	BaseRev    int64
	CurrentRev int64
	ExecID     string
	err        error    // 底层原因，可为 nil
}

func (e *Error) Error() string
func (e *Error) Unwrap() []error   // 按 Code 返回对应哨兵，让 errors.Is 成立
```

调用方重试的形状：`errors.As` 取 `CurrentRev`，重读文件，用新版本重写。不需要解析字符串。

## 8. 第一阶段明确不做

| # | 不做 | 理由 | 何时回来 |
| --- | --- | --- | --- |
| 1 | 远端 / 容器后端与 push/pull | ADR 0006 | 出现远端执行或第二个读者 |
| 2 | 模块后端（goja 等） | 最容易后加 | 需要隔离 JS 时 |
| 3 | data mount（外部来源铺子树） | 没有对应数据源 | 需要预置只读内容时 |
| 4 | git 客户端、artifacts、assets | 外围能力 | 底层稳定后 |
| 5 | agent 工具适配器 | 先有底座再谈上层 | 有第一个真实调用方时 |
| 6 | 多租户鉴权与配额 | 本地单用户部署，鉴权属部署层 | 变多租户时 |
| 7 | 快照 / 时间旅行 / fork | 需要历史，与续传点语义冲突 | 需要审计时 |
| 8 | 硬链接身份、xattr、设备文件 | 用拓扑简单化换实现规模 | 有真实调用方需要 |
| 9 | 内容定义分块 | 固定分块够用 | 出现大量插入型大文件写 |
| 10 | 后台 GC 守护进程 | 改在 `Close` 时与阈值触发 | 出现长时间常驻工作区 |
| 11 | Windows 支持 | 进程组与 FUSE 都要分支，成本翻倍 | 明确有需求时 |

第 7 项与 Q19 的选择绑定：选了续传点，就没有历史可回放。要做快照，先回来改 Q19。
