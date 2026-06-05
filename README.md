# TemplatePoolByGO

基于 Go 泛型的通用连接池，管理 gRPC stream、数据库连接、TCP 长连接等重资源。

```bash
go get github.com/sukasukasuka123/TemplatePoolByGO@latest
```

**要求 Go ≥ 1.23**（使用了泛型类型别名 `type resource[T] = Resource[T]`）。

---

## 架构设计

三个核心决策：

| 决策 | 做法 | 解决的问题 |
|------|------|-----------|
| **Actor 隔离扩缩容** | 扩缩容决策串行跑在独立 eventLoop，Get 热路径只发 CAS 限流信号（10ms 窗口） | 扩容风暴（突发流量时数百 goroutine 同时 Create） |
| **无锁等待队列** | Michael-Scott 变体，每个等待者独立 `chan T`（容量 1），归还时点对点交付，取消时懒惰删除 | 惊群效应 + 取消复杂度 + FIFO 保序 |
| **泛型解耦** | `Conn[T]` 接口四个方法，池子逻辑完全类型安全，零 `interface{}` 断言 | 编译期类型检查 |

### 扩容策略

三阶段非线性曲线 + 压力补偿，Actor 内串行决策：

```
usageRate = (effectiveTotal - MinSize) / (MaxSize - MinSize)

< 20%  → 保守期：每轮 +15
20~75% → 爆发期：每轮 +剩余空间/2
> 75%  → 收敛期：每轮 +剩余空间/8

压力补偿：step = max(曲线步长, 等待人数/3)
         等待 > 100 时：step = max(曲线步长, 等待人数/2)
单次扩容上限：MaxSize / 3
```

### 缩容策略

触发条件：buffer 利用率 > 90% + 无等待者 + 无扩容中 + totalSize > MinSize。每次最多缩 20%，渐进式避免抖动。**超龄连接（SurviveTime）优先驱逐**。

缩容由两种方式触发：`MonitorInterval` 后台 goroutine 定期检查，或 `Get` 时的扩容信号附带检查。

### 等待队列：点对点交付

归还连接时直接 bypass 给等待者——不经过 resources channel：

```
Put → TryDequeue(队头 waiter) → 成功 → 直接交付，零中转
                               → 失败 → 放回 resources channel
```

### 心跳对业务的保护

心跳取连接前检查等待队列，取到一半发现新等待者则放回已取连接。心跳永不会和业务抢连接。

---

## 快速上手

### 1. 实现 `Conn[T]` 接口

```go
type Conn[T any] interface {
    Create() (T, error)   // 建立新连接（扩容时调用，重试次数由 MaxRetries 控制）
    Reset(T) error        // Put 归还前重置（清空缓冲、重置事务等）
    Close(T) error        // 释放连接（缩容/异常/Put 失败时调用）
    Ping(T) error         // 健康检查（心跳 + ReconnectOnGet 时调用）
}
```

### 2. 配置并创建池

```go
cfg := pool.PoolConfig{
    MinSize:          5,               // 启动预热数，永不缩到此以下
    MaxSize:          100,             // 连接总数上限（含使用中 + 空闲）
    IdleBufferFactor: 0.4,             // 空闲连接内存系数（见下方详解）
    SurviveTime:      30 * time.Minute, // 连接最长存活时间，超龄优先驱逐
    MonitorInterval:  10 * time.Second, // 扩缩容定期检查间隔（后台 goroutine）

    // 心跳
    PingInterval: 30 * time.Second,
    OnUnhealthy: func(err error) {
        log.Printf("连接异常: %v", err)
    },

    // 重连
    MaxRetries:     3,              // Create 失败重试次数（扩容 + ReconnectOnGet 均生效）
    RetryInterval:  time.Second,    // 重试间隔
    ReconnectOnGet: false,          // Get 时 Ping 失败是否自动重连（默认关闭）
    MaxWaitQueue:   10000,          // 等待队列上限，超过返回 ErrPoolBusy
}

p := pool.NewPool(cfg, &myConnControl{})
defer p.Close()
```

### 3. 使用

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

res, err := p.Get(ctx)
if err != nil {
    // pool.ErrPoolBusy         — 等待队列满
    // context.DeadlineExceeded — 等待超时
    // context.Canceled         — 调用方主动取消
    // "pool closed"            — 池子已关闭
    return err
}
defer p.Put(res)

// res.Conn 是 T 类型，类型安全，无需断言
// res.ID / res.RetryCount — 可读的连接元数据
task, _ := res.Conn.Send(req)
```

---

## 配置项详解

### `IdleBufferFactor`

最容易被误解的字段：

```
resources channel 容量 = MaxSize × IdleBufferFactor

MaxSize=100, IdleBufferFactor=0.4 → channel 容量 = 40
但 totalSize 仍可达 100（60 个在使用中，不占 channel 槽位）
```

控制的是**空闲连接最多占多少内存**，不影响最大连接数。最小值保护为 1（防无缓冲 channel 死锁）。设为 `1.0` 表示所有连接可同时空闲（最安全但内存最大），设为 `0.3` 表示预期约 70% 连接在使用中。

### `MonitorInterval`

后台 goroutine 每间隔触发一次 `checkAndAdjust`（扩缩容检查 + SurviveTime 驱逐）。设为 0 则关闭定期检查，仅靠 `Get` 信号触发。建议设 5-10 秒。

### `MaxRetries` / `RetryInterval`

**两处生效**：
- **扩容 Create 失败**：重试 `MaxRetries` 次，间隔 `RetryInterval`
- **ReconnectOnGet 重连**：同上参数

如果 `MaxRetries = 0`，底层保护为至少尝试 1 次（防止零值连接泄漏）。最后一次重试后不 sleep。

### `MaxWaitQueue`

等待队列长度上限。当 `waitQueue.Len() >= MaxWaitQueue` 时，`Get` 直接返回 `ErrPoolBusy`，不进入队列。这是一种**熔断机制**——防止上游请求无限堆积导致 OOM。

### `ReconnectOnGet`

设为 `true` 时，每次 `Get` 先 Ping 连接。Ping 成功则交付，Ping 失败则用 `MaxRetries`/`RetryInterval` 重连后交付。**有性能开销**（热路径多一次 Ping），默认关闭。适合连接可用性要求高的场景（如数据库主从切换）。

### `SurviveTime`

连接从 `Create` 算起的最大存活时间。超龄连接在缩容时**优先驱逐**。设为 0 则不禁用驱逐。

### `OnUnhealthy`

Ping 失败时回调。**跑在心跳 goroutine 内，不要做阻塞操作**。适合接入服务发现刷新、告警。

---

## 配置项速查

| 字段 | 类型 | 默认值 | 生效位置 |
|------|------|--------|---------|
| `MinSize` | `int64` | 5 | shrink 下界 |
| `MaxSize` | `int64` | 100 | expand 上界 |
| `IdleBufferFactor` | `float64` | 1.0 | resources channel 容量 |
| `SurviveTime` | `time.Duration` | 30m | shrink 中优先驱逐 |
| `MonitorInterval` | `time.Duration` | 10s | 后台定期 checkAndAdjust |
| `MaxRetries` | `int` | 3 | expand Create + ReconnectOnGet 重连 |
| `RetryInterval` | `time.Duration` | 1s | expand Create + ReconnectOnGet 重连 |
| `ReconnectOnGet` | `bool` | false | validateAndReturn（Get 热路径） |
| `PingInterval` | `time.Duration` | 30s | 心跳 goroutine |
| `OnUnhealthy` | `func(error)` | nil | 心跳 Ping 失败回调 |
| `MaxWaitQueue` | `int64` | 10000 | Get 前置拒绝阈值 |

---

## Resource 元数据

```go
type Resource[T any] struct {
    ID         string    // 连接唯一标识
    Conn       T         // 业务连接（类型安全）
    // RetryCount int     // 重连次数（未导出，调试用）
}
```

---

## Stats 监控

```go
stats, _ := p.Stats(context.Background())
```

| key | 含义 |
|-----|------|
| `total_size` | 当前连接总数 = available + in_use + expanding（近似） |
| `pool_available` | 空闲连接数 |
| `pool_in_use` | 使用中连接数 |
| `waiting_count` | 等待队列长度 |
| `expanding` | 正在建立中的连接数 |
| `buffer_cap` | resources channel 容量 |

**告警规则**：`waiting_count` 持续 > 0 → 池子跟不上请求速度，调大 `MaxSize` 或检查 Create 耗时。

---

## 错误处理

```go
res, err := p.Get(ctx)
switch {
case err == nil:
    // 正常拿到连接

case errors.Is(err, pool.ErrPoolBusy):
    // 等待队列满 → 上游熔断，返回 429 或降级

case errors.Is(err, context.DeadlineExceeded):
    // ctx 超时 → 慢请求日志

case errors.Is(err, context.Canceled):
    // 调用方主动取消 → 通常忽略

default:
    // 其他异常（如 "pool closed"）
}
```

---

## 性能参考

测试环境：Intel i5-1155G7 @ 2.50GHz, Go 1.25, Windows 11

| 场景 | ops/s | 延迟 | 说明 |
|------|-------|------|------|
| 纯调度 Get+Put | 6,340,000 | 193 ns | 0 allocs，池子调度开销上限 |
| 无 sleep 高并发 (1K-20K) | 1,500,000-2,000,000 | ~630 ns | 50 预初始化连接，0 失败 |
| 带 5ms 业务延迟 (100K) | 1,713,000 | +629 ns 池子开销 | 业务延迟主导 |
| Actor SendAsync | 4,800,000 | 370 ns | 扩容信号发送开销 |

详细测试报告见 [docs/test_result/](docs/test_result/)。

---

## 适用场景

**适合**：gRPC stream、数据库连接、TCP 长连接——建立成本高、需要复用的重资源。

**不适合**：byte buffer、临时对象等轻资源 → 用 `sync.Pool`。

---

## 项目文档

```
docs/
├── plan/          ← 修复方案（11 个，按优先级 P0-P3）
├── review/        ← 代码审查 + 设计决策详解
└── test_result/   ← 测试报告（功能 + Benchmark）
```

---

## License

MIT
