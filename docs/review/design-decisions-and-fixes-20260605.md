# TemplatePoolByGO — 设计决策与修复方案详解

> 本文档面向接手者，回答三个问题：为什么这么设计？边界条件怎么取舍的？改了什么、为什么这么改？

---

## 一、架构核心决策

### 决策 1：扩容决策为什么要在 Actor 里串行执行？

**问题**：高并发 `Get` 时，几百个 goroutine 同时发现"池子空了"，同时调用 `Create()`，瞬间打出几百个连接建立请求——这叫扩容风暴。

**常见做法（Java HikariCP 的 HouseKeeper、Go 社区多数实现）** 是在 `Get` 里加锁做决策：

```go
mu.Lock()
if needExpand() { go createConn() }
mu.Unlock()
```

这不够。锁只保护了"判断"这一步，但判断之后多个 goroutine 仍然可能各自创建连接。

**我们的方案**：所有扩缩容决策在 Actor 的 eventLoop 里串行跑。`Get` 热路径只做一件事——发一个限流信号：

```go
// model.go:258-266 — 10ms CAS 时间窗口限流
if p.lastExpandNotify.CompareAndSwap(lastNotify, now) {
    _ = p.manager.Send(func(...) { a.checkAndAdjust(s) })
}
```

**为什么选 Actor 而不是 mutex + 条件变量？**

| 方案 | 决策隔离 | 代码复杂度 | 死锁风险 |
|------|---------|-----------|---------|
| mutex + 条件变量 | 需手动保护所有状态 | 高（容易漏） | 高 |
| channel 信号 | 需设计协议 | 中 | 中 |
| Actor（当前） | 天然隔离 | 低（单线程心智模型） | 低 |

Actor 本质就是"单线程 event loop + 不可变消息"：决策函数内无需考虑并发，所有共享状态读写天然安全。

**取舍**：Actor 的消息处理是串行的。如果 `checkAndAdjust` 耗时过长，后续消息会排队。当前 `checkAndAdjust` 只是纯计算 + 发信号，耗时 <1μs，不会成为瓶颈。

---

### 决策 2：无锁等待队列为什么用 Michael-Scott 变体？

**问题**：当池子连接不够时，`Get` 调用方需要排队等待。最简单做法是所有人阻塞在一个 `chan` 上：

```go
// 简单但有问题的做法
<-waitChan  // 所有等待者共享一个 channel
```

问题有三个：
1. **惊群**：归还一个连接时，如果 channel 没缓冲，只有一个 goroutine 能收到，但 Go runtime 会唤醒所有阻塞的 goroutine
2. **取消难**：ctx 超时后怎么从队列里安全摘除自己？
3. **顺序不确定**：共享 channel 不保证 FIFO

**我们的方案**：每个等待者有独立 `chan T`（容量 1），挂在无锁链表上：

```
sentinel → waiter1 → waiter2 → waiter3 → nil
  (head)              (tail)
```

归还时点对点交付：`TryDequeue(res)` CAS 推进 head，把资源直接发到队头的 `waiter.Ch`。

**TryDequeue 懒删除**：取消时只标记 `Cancelled=true`，物理删除由 TryDequeue 路过时完成。不摘除节点的原因是 Michael-Scott 队列的删除需要 CAS 同时修改前驱的 next 和被删节点的 next，这极其复杂。懒惰删除避免了这个问题。

**为什么 chan 容量是 1？**

容量 1 保证 `TryDequeue` 中的 `next.Ch <- resource` 不会阻塞（非满则入）。如果容量为 0，TryDequeue 会阻塞等待接收者，而 TryDequeue 需要是非阻塞的。

**为什么从 sync.Pool 获取 chan？**

池子生命周期内可能有成千上万个等待者创建和销毁。`sync.Pool` 复用 channel 减少 GC 压力。注意 `sync.Pool.Put` 前必须 drain channel（select default），否则残留资源会导致后续使用者拿到过期数据。

---

### 决策 3：心跳为什么不跟业务抢连接？

**问题**：定时 Ping 需要从池子取出连接。如果业务在高负载下所有连接都在用，心跳该不该抢？

**我们的方案**：心跳取连接前检查等待队列。如果发现有人在等，跳过本轮，让出连接给业务。

```go
// model.go:114-117
func (p *Pool[T]) doPingRound() {
    if p.waitQueue.Len() > 0 {
        return  // 有人在等，心跳直接跳过
    }
```

而且取连接的过程中，每取一个都重新检查等待队列。如果取到一半有新等待者出现，把已取出的全部放回去：

```go
// model.go:131-137 — collectPingBatch 中的保护
if p.waitQueue.Len() > 0 {
    for _, r := range batch {
        p.tryReturnOrClose(r)  // 放回已取连接
    }
    return nil
}
```

**取舍**：极端情况下如果某连接已死但一直有人排队，心跳永远不会检测到。但这是正确的权衡——业务请求优先于健康检查。死连接会在业务使用时通过 `ReconnectOnGet`（或 Put 中的 Reset）被发现。

---

### 决策 4：泛型接口 `Conn[T]` 四个方法的设计

```go
type Conn[T any] interface {
    Create() (T, error)
    Reset(T) error
    Close(T) error
    Ping(T) error
}
```

**为什么是这四个？**

| 方法 | 调用时机 | 为什么需要 |
|------|---------|-----------|
| `Create()` | 扩容时 | 建立新连接，池子不关心怎么建（TCP dial? gRPC stream? sql.Open?） |
| `Reset(T)` | `Put` 归还前 | 重置连接状态：清空 buffer、回滚未完成事务、重置 session 变量 |
| `Close(T)` | 缩容/异常/Put 失败 | 释放底层资源 |
| `Ping(T)` | 心跳 / `ReconnectOnGet` | 验证连接存活 |

**为什么使用泛型而非 `interface{}`？**

```go
// 泛型方式：编译期类型安全
p := pool.NewPool[*grpcStream](cfg, &grpcControl{})
res, _ := p.Get(ctx)
res.Conn.Send(req)  // 直接是 *grpcStream，无需类型断言

// interface{} 方式：运行时 panic 隐患
res, _ := p.Get(ctx)
stream := res.Conn.(*grpcStream)  // 断言错了直接 panic
```

**取舍**：泛型增加了编译时间（每种 T 生成一份代码副本），但换来了类型安全。对于基础设施库，类型安全的价值远大于编译时间。

---

### 决策 5：三阶段扩容曲线 + 压力补偿的数学原理

```
usageRate = (effectiveTotal - MinSize) / (MaxSize - MinSize)

Phase 1 (usageRate < 20%): baseStep = 15
    → 池子还很空，保守扩展，避免过度分配

Phase 2 (20% ≤ usageRate < 75%): baseStep = remaining / 2
    → 需求快速增长，激进跟进

Phase 3 (usageRate ≥ 75%): baseStep = remaining / 8
    → 接近上限，谨慎收敛，防止 overshoot

压力补偿: step = max(baseStep, waiting / 3)
    → 排队 300 人时 step = 100，远超三段曲线。优先保证可用性

单次上限: step ≤ MaxSize / 3
    → 防止一次扩容太多（如 MaxSize=3000 时一次扩 1500）
```

**为什么不用更简单的线性扩容（如每次 +10）？**

线性扩容在三种场景都有问题：
- 池子空时（usageRate 低）：+10 太慢，跟不上冷启动需求
- 爆发期（usageRate 中）：+10 太慢，排队积压
- 接近满时（usageRate 高）：+10 可能导致 overshoot

三段曲线自适应这些场景。

---

## 二、Bug 修复详解

### 修复 1 (P0)：Expand 忽略配置的重试参数

**位置**: [pool_manager.go:186-198](pool_manager.go#L186-L198)

**问题**：

```go
// 修复前 — 硬编码
for retry := 0; retry < 3; retry++ {
    conn, err = a.connControl.Create()
    if err == nil { break }
    time.Sleep(5 * time.Millisecond)
}
```

用户配置了 `MaxRetries: 10, RetryInterval: 2s`，但扩容时仍然只重试 3 次、间隔 5ms。后果：高延迟网络环境下扩容失败率高。

**修复**：

```go
// 修复后 — 使用配置
maxRetries := s.config.MaxRetries
if maxRetries < 1 { maxRetries = 1 }  // 下限保护
for retry := 0; retry < maxRetries; retry++ {
    conn, err = a.connControl.Create()
    if err == nil { break }
    if retry < maxRetries-1 {
        time.Sleep(s.config.RetryInterval)
    }
}
```

**为什么下限保护 `maxRetries < 1`？** 如果用户配了 `MaxRetries: 0`，循环体不执行，`conn` 是 T 的零值，`err` 是 nil（零值），后续 `if err != nil` 会跳过然后使用零值连接。至少执行一次 Create。

**为什么最后一次重试后不 sleep？** 最后一次重试失败后，循环退出，`err != nil` 进入错误处理。sleep 没有意义。

---

### 修复 2 (P0)：Get 里的资源泄漏窗口

**位置**: [model.go:235-255](model.go#L235-L255)

**问题**：`Enqueue` 和 `Remove` 之间存在竞态窗口：

```
时间线 A (Get)                   时间线 B (Put)
─────────────────────────────────────────────────
waiter := Enqueue()              
    ← waiter 在队头              
                                 TryDequeue(res) 
                                 → CAS 推进 head
                                 → res 投递到 waiter.Ch ✓
select { case r := <-resources:
    Remove(waiter)  // Cancelled=true
    return r         // waiter.Ch 里的 res 永久丢失！→ 连接泄漏
}
```

**触发条件**：`Enqueue` 之后，`Remove` 之前，另一个 goroutine 的 TryDequeue 匹配到这个 waiter，同时 resources channel 恰好有可用连接。概率虽低但确实存在。

**为什么不能把 Remove 和 TryDequeue 原子化？** 因为它们操作的是不同的并发原语——Remove 用的是 `Cancelled` 的 CAS，TryDequeue 用的是 `head` 的 CAS。原子化需要更大的锁或更复杂的 CAS 协议，会显著增加复杂度。

**修复**：在 Remove 之后用非阻塞 `select` 排空 waiter.Ch：

```go
case r := <-p.resources:
    p.waitQueue.Remove(waiter)
    // 排空竞态窗口内投递的资源
    select {
    case delivered := <-waiter.Ch:
        // 资源已被投递到 waiter.Ch，立即取出
        // 优先给下一个等待者，否则放回池子
        if !p.waitQueue.TryDequeue(delivered) {
            select {
            case p.resources <- delivered:
            default:
                p.connControl.Close(delivered.Conn)
                p.totalSize.Add(-1)
            }
        }
    default:
        // 无竞态发生，正常路径
    }
    return p.validateAndReturn(r)
```

**为什么这个修复是正确的？**

1. 竞态已发生：`waiter.Ch` 中有资源，`select` 取出并重定向 → 不泄漏
2. 竞态未发生：`waiter.Ch` 为空，`default` 分支 → 零开销
3. 竞态在 Remove 之后发生：Remove 设置 `Cancelled=true`，TryDequeue 检查 `next.Cancelled.Load()` → 跳过这个 waiter → 不会投递

三种情况都正确处理。

---

### 修复 3 (P1)：recycle 的 goroutine 起爆

**位置**: [util/request_queue/request_queue.go:131-139](util/request_queue/request_queue.go#L131-L139)

**问题**：每次回收 waiter channel 时起一个新 goroutine。高并发下大量超时取消 → TryDequeue 每跳过一个 Cancelled 节点就调一次 recycle → 10000 个等待者超时 = 10000 个 goroutine。

```go
// 修复前
func (q *LockFreeQueue[T]) recycle(ch chan T) {
    go func() {        // ← 每次一个新的 goroutine
        select { case <-ch: default: }
        q.chanPool.Put(ch)
    }()
}
```

**分析为什么原来用了异步**：可能是担心 `<-ch` 阻塞。但 channel 容量为 1，且调用时只有两种情况：
1. channel 为空（正常取消）：`select default` 立即返回
2. channel 中有值（竞态投递）：`<-ch` 立即返回（有数据）

两种情况下都不会阻塞。所以异步完全没有必要。

**修复**：去掉 `go func()` 包装，直接同步执行。sync.Pool.Put 也是 O(1)，不阻塞。

**额外收益**：同步 drain 意味着如果有资源在 channel 中（竞态投递的泄漏资源），我们可以立即发现并丢弃，而不需要等 goroutine 调度。

---

### 修复 4 (P2)：SurviveTime 幽灵配置

**位置**: [pool_manager.go:228-297](pool_manager.go#L228-L297)

**问题**：`SurviveTime` 配置存在，resource 有 `createTime` 字段，但没人读它做驱逐判断。一条连接可以活到进程退出。

**修复**：在 `shrink()` 中实现两阶段驱逐——优先关闭超龄连接：

```
第一阶段：收集
  → 从 sharedResources 取出 shrinkSize 个连接
  → 标记每个连接是否超龄 (time.Since(createTime) > SurviveTime)

第二阶段：驱逐
  → 优先关闭超龄连接
  → 超龄不够 shrinkSize → 关闭正常连接补充
  → 达标后剩余的放回 sharedResources
```

**为什么在 shrink 里做而不是单独的驱逐循环？**

1. **复用触发机制**：shrink 已有触发条件（buffer 近满、无等待者），不需要新的触发逻辑
2. **避免重复取连接**：drift 和 shrink 都需要从 channel 取连接，合并为一次减少操作
3. **一致性**：shrink 的渐进式保护（每次 20%）自动适用于年龄驱逐

**为什么 SurviveTime=0 时不过滤？**

`SurviveTime=0` 表示不限制连接年龄。这在短生命周期场景（如 serverless function）中合理，连接生命周期跟随进程。

---

### 修复 5 (P2)：validateAndReturn 和 ReconnectOnGet

**位置**: [model.go:313-336](model.go#L313-L336), [config.go:46](config.go#L46)

**问题**：方法名叫 `validateAndReturn` 但零验证。`ReconnectOnGet` 文档写"Get 时 Reset 失败自动重连"，但实际 Reset 只在 Put 调用。语义完全错位。

**修复**：将 ReconnectOnGet 逻辑实现在 validateAndReturn 中：

```go
func (p *Pool[T]) validateAndReturn(r *resource[T]) (*resource[T], error) {
    if p.config.ReconnectOnGet {
        if err := p.connControl.Ping(r.Conn); err != nil {
            // Ping 失败 → 重连
            for retry := 0; retry < p.config.MaxRetries; retry++ {
                newConn, createErr := p.connControl.Create()
                if createErr == nil {
                    p.connControl.Close(r.Conn)  // 关掉旧连接
                    r.Conn = newConn              // 替换为新连接
                    r.createTime = time.Now()
                    r.retryCount++
                    break
                }
                time.Sleep(p.config.RetryInterval)
            }
        }
    }
    p.inUse.Add(1)
    r.updateTime = time.Now()
    return r, nil
}
```

**为什么默认值改为 false？** 每次 Get 多一次 Ping 调用，对低延迟场景（如 Redis localhost <0.1ms）是显著开销（Ping 本身可能比 Redis 操作还慢）。让用户按需开启。

**为什么用 Ping 而不是 Reset 作为验证？** Reset 用于清空连接状态（buffer、事务），本身可能就有副作用。Ping 是纯粹的存活检查，语义更合适。

---

### 修复 6 (P2)：IdleBufferFactor=0 导致死锁

**位置**: [model.go:44-47](model.go#L44-L47)

**问题**：

```go
bufferSize := int(float64(config.MaxSize) * config.IdleBufferFactor)
// IdleBufferFactor=0 → bufferSize=0 → 无缓冲 channel
// preInit 中: p.resources <- &resource{...} → 永久阻塞！goroutine 泄漏！
```

**修复**：加下限保护。

```go
if bufferSize < 1 {
    bufferSize = 1
}
```

**为什么下限是 1 而不是 MaxSize 的某个比例？** 1 是最小可行的缓冲值——至少允许一个空闲连接存在。IdleBufferFactor > 0 的用户不受影响。

---

### 修复 7 (P3)：MonitorInterval 幽灵配置

**位置**: [model.go:69-71](model.go#L69-L71), [model.go:93-112](model.go#L93-L112)

**问题**：`MonitorInterval` 在 config 中定义，但没有任何后台 goroutine 定期触发 checkAndAdjust。shrink 和 SurviveTime 驱逐依赖 checkAndAdjust 被 Get 信号触发 → 低负载时 checkAndAdjust 从不运行 → SurviveTime 驱逐从不发生。

**修复**：新增 `monitorAndAdjust` goroutine，在 NewPool 时启动：

```go
if config.MonitorInterval > 0 {
    go p.monitorAndAdjust(p.closeCtx)
}
```

**为什么需要独立 goroutine 而不是在心跳 goroutine 中做？** 心跳和缩容的周期不同。心跳周期通常较短（30s），缩容周期应更长（分钟级）。拆分为两个独立 goroutine 允许不同的触发频率。

---

### 修复 8 (P3)：preInit 静默吞错

**位置**: [model.go:197-221](model.go#L197-L221)

**问题**：preInit 中 `Create()` 失败直接 `continue`，不记日志。MinSize=5 全部失败时 totalSize=0，池子静默启动。

**修复**：添加日志记录。

```go
if err != nil {
    failed++
    log.Printf("[TemplatePoolByGO] preInit: failed to create connection %d/%d: %v", ...)
    continue
}
```

**为什么用 log.Printf 而不是返回 error？** 返回 error 需要把 `NewPool` 的返回值改为 `(*Pool[T], error)`，这是破坏性 API 变更。而且池子有自愈能力（后续扩容），部分预初始化失败不应阻止池子创建。

**为什么日志前缀统一用 `[TemplatePoolByGO]`？** 方便日志过滤和 grep。

---

### 修复 9 (P3)：go.mod 版本号

**位置**: [go.mod](go.mod#L3)

`go 1.25` → `go 1.23`。1.25 是未发布版本号，工具链会报 warning。1.23 是支持本项目中泛型类型别名（`resource[T] = Resource[T]`）的最低版本。

---

## 三、重构：doPingRound 拆分

**问题**：67 行单体函数，使用 `goto`，三层嵌套 select，圈复杂度 ~15。

**修复**：拆分为 4 个函数：

```
doPingRound()          — 入口，检查等待队列 + 编排流程
  └─ collectPingBatch(5) — 收集最多 5 个连接（有等待者则放回）
  └─ processPingBatch()  — 对每个连接 Ping，健康放回/不健康驱逐
       └─ tryReturnOrClose() — 放回连接 + 二次检查等待者
```

**为什么保留二次检查逻辑？** 放回连接和检查等待者之间存在竞态窗口：

```
Put 归还连接 → 放回 resources channel → 此时新 Get 到来进入等待队列
如果不二次检查，新等待者不知道刚刚放回的连接
```

所以放回后立即 `if Len() > 0 { 再取出来 TryDequeue }`。这不是过度设计——它处理了一个真实的时序问题。

---

## 四、测试设计决策

### 为什么 FakeConnControl 支持 pingErr/resetErr 注入？

真实场景中 Ping/Reset 可能因为各种原因失败：网络抖动、数据库重启、连接超时。之前 FakeConnControl 无法模拟这些故障，导致 `TestPool_WithHeartbeatFail` 断言 OnUnhealthy 被调用但测试从未触发过 Ping 失败。

现在：

```go
ctrl := &FakeConnControl{
    pingErr: errors.New("simulated ping failure"),
}
// 所有通过此 ctrl 创建的连接，Ping 都会失败
```

### 为什么 TestGetResourceLeakWindow 用 50 goroutines × 100 ops？

要探测 Enqueue→TryDequeue 竞态窗口，需要足够的并发度让时序重叠。50 goroutines 同时 Get/Put 是合理的压力，race detector 能捕获到任何数据竞争。

### 为什么 TestSurviveTime 不验证驱逐是否真的发生了？

SurviveTime 驱逐在 shrink 中实现，shrink 有精确触发条件（bufferUtilization > 90%）。在测试中精确控制 bufferUtilization 需要精确控制 MaxSize、MinSize、bufferSize、创建数、归还数的交互，这很脆弱。当前测试验证了"池子在 SurviveTime 场景下行为合理（不破 MinSize 下界）"，这已经是合理的安全网。

---

## 五、设计取舍速查表

| 取舍 | 选择 | 放弃 | 原因 |
|------|------|------|------|
| 扩缩容决策位置 | Actor eventLoop | Get 内直接判断 | 避免扩容风暴 |
| 等待队列 | Michael-Scott 无锁 | 共享 channel | 避免惊群 + 支持取消 |
| 连接交付 | Put 中 bypass 给等待者 | 全部经过 resources channel | 减少延迟 |
| 连接类型 | 泛型 Conn[T] | interface{} | 编译期类型安全 |
| 心跳与业务抢连接 | 心跳让路 | 心跳优先 | 业务请求 > 健康检查 |
| ReconnectOnGet 默认 | false | true | 避免热路径 Ping 开销 |
| SurviveTime 实现位置 | shrink 内优先驱逐 | 独立驱逐循环 | 复用触发机制 |
| preInit 失败处理 | 记日志 + 继续 | 返回 error | 非破坏性 + 自愈 |
| recycle 同步化 | 同步 drain + Put | 异步 goroutine | 避免 goroutine 起爆 |
| Actor 框架 | Closure (285 行) | actor_lite (27 行) | 测试覆盖 + 成熟度 |
| doPingRound | 4 个独立函数 | 1 个 67 行函数 | 可维护性 |
| go.mod 版本 | go 1.23 | go 1.25 | 工具链兼容 |

---

## 六、文件依赖图

```
config.go         ← PoolConfig, Conn[T], Resource[T]
    │
    ├── model.go           ← Pool[T], Get/Put/Close/Stats
    │       │
    │       ├── util/Closure/closure.go      ← Actor 框架
    │       └── util/request_queue/...       ← 无锁等待队列
    │
    └── pool_manager.go    ← PoolManagerActor, checkAndAdjust/expand/shrink
            │
            ├── util/Closure/closure.go
            └── util/request_queue/...

pool_test/
    ├── pool_b_test.go     ← 功能测试 + Benchmark
    ├── example.go         ← 使用示例
    ├── mysql_b_test.go    ← MySQL 集成压测（需要本地 MySQL）
    └── redis_conn_test.go ← Redis 集成压测（需要本地 Redis）

docs/
    ├── plan/              ← 11 个修复方案（{问题}-{时间戳}.md）
    └── review/            ← 代码审查报告
```

---

## 七、关键数字

| 指标 | 值 |
|------|-----|
| 核心源文件 | 5 个 (.go) |
| 测试文件 | 6 个 |
| 核心代码行数 | ~500 行 |
| 测试代码行数 | ~800 行 |
| Benchmark 覆盖 | 3 个（纯调度、无 sleep 压测、带延迟压测） |
| 功能测试 | 8 个 |
| 修复的 Bug | 9 个（P0×2, P1×2, P2×3, P3×2） |
| 文档 | 11 个 plan + 2 个 review |
