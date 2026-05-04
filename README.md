# TemplatePoolByGO 项目介绍

**English Documentation**  
See the full English README here:  
[README_EN.md](./README_EN.md)

一个基于泛型的 Go 连接池，核心目标是**管理重资源**（gRPC stream、数据库连接、TCP 连接等）。

设计上有三个核心取舍：用 Actor 把扩缩容决策从热路径里隔离出去；用无锁等待队列做等待者管理；用泛型把池子逻辑和连接类型彻底解耦。

```bash
go get github.com/sukasukasuka123/TemplatePoolByGO@v0.1.7
```

---

## 你当时为什么这么设计

### 问题一：扩容风暴

常见池子在 `Get` 里直接判断要不要扩容：

```go
// 常见做法，有问题
mu.Lock()
if totalSize < maxSize {
    go createNewConn()  // 突发流量时，很多 goroutine 同时触发
}
mu.Unlock()
```

突发流量打来时，几百个 goroutine 同时判断"需要扩容"，同时去 `Create`，瞬间打出几百个连接建立请求——这叫扩容风暴。

**你的做法**：`Get` 里只发一个限流信号（CAS + 时间窗口），所有扩缩容决策串行跑在一个 Actor 的 eventLoop 里。

```go
// Get 里只做这件事
if p.lastExpandNotify.CompareAndSwap(lastNotify, now) {
    _ = p.manager.Send(func(...) { a.checkAndAdjust(s) })
    // 信号丢进 actor inbox，决策只发生一次
}
```

扩容逻辑天然无竞争，不需要任何锁。

### 问题二：等待者管理

连接不够时调用方需要排队等待。最简单的做法是让所有人阻塞在同一个 channel 上，但这样"先来先得"无法保证，而且取消等待（ctx 超时）很麻烦。

**你的做法**：每个等待者有自己独立的 `chan T`，挂在一个无锁链表上。连接归还时直接点对点交付给队头等待者，跳过 `resources` channel。

```go
// Put 里
if p.waitQueue.TryDequeue(res) {
    // 连接直接送到等待者手里，不经过 resources channel
    return nil
}
```

等待者取消时只是标记节点为 `Cancelled`，下次 `TryDequeue` 路过时懒删除，不需要从链表里真正摘除。

### 问题三：接入不同连接类型

大多数池子写死了连接类型，或者用 `interface{}` + 类型断言。**你用泛型**：

```go
type Conn[T any] interface {
    Create() (T, error)
    Reset(T)  error
    Close(T)  error
    Ping(T)   error
}
```

池子的全部逻辑是类型安全的。换连接类型只需要换一个 `Conn[T]` 实现。

---

## 快速上手

### 第一步：实现 `Conn[T]` 接口

四个方法，对应连接的完整生命周期：

| 方法 | 时机 | 说明 |
|------|------|------|
| `Create()` | 扩容时 | 建立一条新连接 |
| `Reset(T)` | `Put` 时 | 归还前重置连接状态（清空缓冲区、重置事务等） |
| `Close(T)` | 缩容 / 连接异常时 | 释放连接 |
| `Ping(T)` | 心跳时 | 检查连接是否健康 |

以 gRPC stream 为例（取自 microHub 项目）：

```go
type streamResource struct {
    sp *StreamPool
}

func (r *streamResource) Create() (*SingleStream, error) {
    stream, err := r.sp.client.DispatchStream(context.Background())
    if err != nil {
        return nil, err
    }
    return NewSingleStream(stream, r.sp.addr, nil), nil
}

func (r *streamResource) Reset(s *SingleStream) error {
    if s.IsClosed() {
        return fmt.Errorf("stream already closed")
    }
    return nil
}

func (r *streamResource) Close(s *SingleStream) error {
    s.Close()
    return nil
}

func (r *streamResource) Ping(s *SingleStream) error {
    if s.IsClosed() {
        return fmt.Errorf("stream closed")
    }
    // 也可以检查底层 gRPC conn 的状态
    state := r.sp.Conn.GetState()
    if state == connectivity.Shutdown || state == connectivity.TransientFailure {
        return fmt.Errorf("conn unhealthy: %s", state)
    }
    return nil
}
```

### 第二步：配置并创建池子

```go
cfg := pool.PoolConfig{
    MinSize:          5,      // 启动时预热 5 条连接，池子永远不缩到 5 以下
    MaxSize:          100,    // 连接总数上限（含使用中 + 空闲）
    IdleBufferFactor: 0.4,    // resources channel 容量 = MaxSize × 0.4 = 40
                              // 注意：这只影响空闲槽位的内存占用，不影响 MaxSize

    SurviveTime:     30 * time.Minute, // 连接最长存活时间
    MonitorInterval: 10 * time.Second, // 缩容检查间隔
    MaxWaitQueue:    10000,            // 等待队列上限，超过返回 ErrPoolBusy

    // 心跳配置
    PingInterval: 30 * time.Second,
    OnUnhealthy: func(err error) {
        // Ping 失败、连接被驱逐时触发
        // 可以在这里接入服务发现的刷新逻辑
        log.Printf("连接异常被驱逐: %v", err)
    },

    // 重连配置
    MaxRetries:     3,            // Create 失败时的重试次数
    RetryInterval:  time.Second,  // 重试间隔
    ReconnectOnGet: true,         // Get 时 Reset 失败是否自动重连
}

p := pool.NewPool(cfg, &streamResource{sp: myStreamPool})
defer p.Close()
```

### 第三步：使用

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

res, err := p.Get(ctx)
if err != nil {
    // pool.ErrPoolBusy            — 等待队列满了，直接拒绝
    // context.DeadlineExceeded    — 等待超时
    // context.Canceled            — 调用方主动取消
    // "pool closed"               — 池子已关闭
    return err
}
defer p.Put(res)  // 用完一定要还，否则连接泄漏

// res.Conn 就是你的 *SingleStream（或其他 T 类型）
task, err := res.Conn.Send(req)
```

---

## 配置项说明

### `IdleBufferFactor` 是什么

这个字段容易误解，单独说清楚。

```
resources channel 的容量 = MaxSize × IdleBufferFactor

MaxSize=100, IdleBufferFactor=0.4
→ channel 容量 = 40
→ 但 totalSize 仍然可以到 100（另外 60 个连接在使用中，不占 channel 槽位）
```

`IdleBufferFactor` 控制的是**空闲连接最多占多少内存**，不影响最大连接数。设为 `1.0` 表示所有连接都可以同时空闲（内存最大但最安全）；设为 `0.4` 表示预期大约 60% 的连接会在使用中。

### `OnUnhealthy` 怎么接入服务发现

```go
OnUnhealthy: func(err error) {
    // Ping 的实现里可以把失败原因编码进 error
    if isConnLevelFailure(err) {
        // gRPC 连接彻底断了 → 通知服务发现重新解析地址，重建整个 StreamPool
        serviceDiscovery.TriggerRefresh(addr)
    }
    // stream 级别的失败不需要处理
    // 池子的 checkAndAdjust 会自动补充新 stream
},
```

回调跑在心跳 goroutine 里，**不要在里面做阻塞操作**，重的工作起一个 goroutine 或者发到 channel 里。

---

## 内部机制

### 扩容：三阶段非线性曲线

扩容步长根据当前使用率动态计算：

```
usageRate = (totalSize - MinSize) / (MaxSize - MinSize)

< 20%  → 保守期：每轮 +15
20~75% → 爆发期：每轮 +剩余空间/2
> 75%  → 收敛期：每轮 +剩余空间/8

压力补偿：step = max(曲线步长, 等待人数/3)
         等待人数 > 100 时：step = max(曲线步长, 等待人数/2)

单次扩容上限：MaxSize/3
```

触发条件：有人在排队 **且** `totalSize < MaxSize`，或者 resources channel 利用率低于 30%（说明大多数连接都在使用中）。

### 缩容

触发条件：channel 利用率 > 90%（空闲连接很多）**且** 没有等待者 **且** `totalSize > MinSize`。

每轮最多缩掉超出 `MinSize` 部分的 20%，渐进式缩容避免抖动。

### 心跳对业务的保护

```go
func (p *Pool[T]) doPingRound() {
    if p.waitQueue.Len() > 0 {
        return  // 有人在等连接，心跳直接跳过这轮
    }
    for i := 0; i < 5; i++ {
        if p.waitQueue.Len() > 0 {
            // 取出连接到一半，发现有新的等待者进来了
            // 把已取出的全部放回去，让出连接
            for _, r := range batch { p.resources <- r }
            return
        }
        // ...
    }
}
```

心跳永远不会和业务请求抢连接。

### 等待者的无锁队列

```
入队：Enqueue() 返回一个 *LockFreeWaiter，里面有一个 buffered chan T
取消：Remove(waiter) 只标记 Cancelled=true，不做物理删除
交付：TryDequeue(res) CAS 推进队头，跳过已取消的节点，把 res 发到队头的 chan 里
```

队列是 Michael-Scott 无锁队列的变体，用哨兵节点统一头尾操作。

---

## 错误处理

```go
res, err := p.Get(ctx)
switch {
case err == nil:
    // 正常拿到连接

case errors.Is(err, pool.ErrPoolBusy):
    // 等待队列满了（waitQueue.Len() >= MaxWaitQueue）
    // 说明系统已经过载，直接向上层返回 429 或降级

case errors.Is(err, context.DeadlineExceeded):
    // 在等待队列里等太久，ctx 超时
    // 可以记录慢请求日志

case errors.Is(err, context.Canceled):
    // 调用方主动取消，通常不需要特殊处理

default:
    // "pool closed" 或其他异常
}
```

---

## 监控

```go
stats, _ := p.Stats(context.Background())
```

| key | 含义 |
|-----|------|
| `total_size` | 当前管理的连接总数（使用中 + 空闲） |
| `pool_available` | resources channel 里的空闲连接数 |
| `pool_in_use` | 当前被取出使用的连接数 |
| `waiting_count` | 正在等待的调用方数量 |
| `expanding` | 正在建立中（还没加入池子）的连接数 |
| `buffer_cap` | resources channel 的容量（= MaxSize × IdleBufferFactor） |

`total_size = pool_available + pool_in_use + expanding`（近似，有极短的中间态）

高负载时重点关注 `waiting_count`：持续 > 0 说明池子跟不上请求速度，考虑调大 `MaxSize` 或检查连接建立耗时。

---

## 适用场景

**适合**：gRPC stream、数据库连接、TCP 长连接——任何建立成本高、需要复用的重资源。

**不适合**：byte buffer、临时对象这类极轻资源，用标准库的 `sync.Pool` 更合适（per-P 分片，GC 友好，开销更低）。

---

## License

MIT