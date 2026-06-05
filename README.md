# TemplatePoolByGO

一个基于 Go 泛型的通用连接池，管理 gRPC stream、数据库连接、TCP 连接等重资源。

```bash
go get github.com/sukasukasuka123/TemplatePoolByGO@latest
```

---

## 架构设计

三个核心决策：

| 决策 | 做法 | 解决的问题 |
|------|------|-----------|
| **Actor 隔离扩缩容** | 扩缩容决策串行跑在独立 eventLoop，`Get` 热路径只发限流信号 | 扩容风暴（突发流量时数百 goroutine 同时 Create） |
| **无锁等待队列** | Michael-Scott 变体，每个等待者独立 `chan T`，归还时点对点交付 | 惊群效应 + 取消复杂度 |
| **泛型解耦** | `Conn[T]` 接口四个方法，池子逻辑完全类型安全 | `interface{}` 的类型断言风险 |

### 扩容策略

三阶段非线性曲线 + 压力补偿，Actor 内串行决策，天然无锁：

```
usageRate = (totalSize - MinSize) / (MaxSize - MinSize)

< 20%  → 保守期：每轮 +15
20~75% → 爆发期：每轮 +剩余空间/2
> 75%  → 收敛期：每轮 +剩余空间/8

压力补偿：step = max(曲线步长, 等待人数/3)
         等待 > 100 时：step = max(曲线步长, 等待人数/2)
单次扩容上限：MaxSize / 3
```

### 缩容策略

触发条件：buffer 利用率 > 90% + 无等待者 + totalSize > MinSize。每次最多缩 20%，渐进式避免抖动。**超龄连接（SurviveTime）优先驱逐**。

### 心跳对业务的保护

心跳取连接前检查等待队列，取到一半发现新等待者则放回已取连接。心跳永不会和业务抢连接。

---

## 快速上手

### 1. 实现 `Conn[T]` 接口

```go
type Conn[T any] interface {
    Create() (T, error)   // 建立新连接
    Reset(T) error        // Put 归还前重置（清空缓冲、重置事务等）
    Close(T) error        // 释放连接
    Ping(T) error         // 健康检查（心跳/ReconnectOnGet 时调用）
}
```

### 2. 配置并创建池

```go
cfg := pool.PoolConfig{
    MinSize:          5,               // 启动预热数，池子不缩到此以下
    MaxSize:          100,             // 连接总数上限
    IdleBufferFactor: 0.4,             // 空闲连接内存系数（见下方详解）
    SurviveTime:      30 * time.Minute, // 连接最长存活时间，超龄优先驱逐
    MonitorInterval:  10 * time.Second, // 缩容/扩容定期检查间隔

    // 心跳
    PingInterval: 30 * time.Second,
    OnUnhealthy: func(err error) {
        log.Printf("连接异常: %v", err)
    },

    // 重连
    MaxRetries:     3,              // Create 失败重试次数（扩容时生效）
    RetryInterval:  time.Second,    // 重试间隔
    ReconnectOnGet: false,          // Get 时 Ping 失败自动重连（默认关闭）
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
    return err
}
defer p.Put(res)

// res.Conn 是 T 类型，类型安全
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

控制的是**空闲连接最多占多少内存**，不影响最大连接数。最小值为 `max(1, MaxSize × factor)`，防止无缓冲 channel 死锁。

### `MonitorInterval`

后台 goroutine 定期触发 `checkAndAdjust`（缩容/扩容检查）。设为 0 则关闭定期检查（仅靠 `Get` 信号触发）。建议设 5-10 秒。

### `ReconnectOnGet`

设为 `true` 时，每次 `Get` 会先 Ping 连接，Ping 失败则自动重连。**有性能开销**（热路径多一次 Ping），默认关闭。适合对连接可用性要求极高的场景。

### `SurviveTime`

连接从 `Create` 算起的最大存活时间。超龄连接在缩容时**优先驱逐**（在 `shrink` 中实现）。设为 0 则不禁用驱逐。

### `OnUnhealthy`

Ping 失败时回调，**跑在心跳 goroutine 内，不要做阻塞操作**。适合接入服务发现刷新、告警等。

---

## 配置项速查

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `MinSize` | `int64` | 5 | 最小连接数，shrink 不下于此 |
| `MaxSize` | `int64` | 100 | 最大连接数（含使用中） |
| `IdleBufferFactor` | `float64` | 1.0 | 空闲连接 buffer 系数 |
| `SurviveTime` | `time.Duration` | 30m | 连接最长存活时间 |
| `MonitorInterval` | `time.Duration` | 10s | 定期扩缩容检查间隔 |
| `MaxRetries` | `int` | 3 | 创建连接失败重试次数 |
| `RetryInterval` | `time.Duration` | 1s | 重试间隔 |
| `ReconnectOnGet` | `bool` | false | Get 时 Ping 失败自动重连 |
| `PingInterval` | `time.Duration` | 30s | 心跳间隔 |
| `OnUnhealthy` | `func(error)` | nil | 连接异常回调 |
| `MaxWaitQueue` | `int64` | 10000 | 等待队列上限 |

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

高负载时关注 `waiting_count`：持续 > 0 → 池子跟不上，调大 `MaxSize` 或检查创建耗时。

---

## 错误处理

```go
res, err := p.Get(ctx)
switch {
case err == nil:
    // 正常

case errors.Is(err, pool.ErrPoolBusy):
    // 等待队列满 → 返回 429 或降级

case errors.Is(err, context.DeadlineExceeded):
    // ctx 超时 → 慢请求日志

case errors.Is(err, context.Canceled):
    // 调用方取消 → 通常忽略

default:
    // 其他异常（如 "pool closed"）
}
```

---

## 适用场景

**适合**：gRPC stream、数据库连接、TCP 长连接——建立成本高、需要复用的重资源。

**不适合**：byte buffer、临时对象等轻资源 → 用 `sync.Pool`（per-P 分片，GC 友好）。

---

## License

MIT
