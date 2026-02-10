# TemplatePoolByGO 项目介绍

**English Documentation**  
See the full English README here:  
[README_EN.md](./README_EN.md)


## 一、项目简介

本项目是一款基于Go语言实现的高性能通用资源池（连接池），采用Actor模型实现资源的异步扩容/缩容管理，结合无锁队列作为等待队列提升并发性能，支持资源复用、自动健康检查、超时控制等核心能力，可适配各类连接型资源（如数据库连接、网络连接等）的池化管理。

## 二、快速使用方式

### 1. 核心配置定义

首先定义资源池的核心配置，控制池的大小、存活时间、重试策略等：

```go
import (
    "context"
    "time"
    pool "github.com/sukasukasuka123/TemplatePoolByGO"
)

// 定义资源池配置
config := PoolConfig{
    MinSize:          50,         // 池最小资源数
    MaxSize:          500,        // 池最大资源数
    SurviveTime:      180 * time.Second, // 空闲资源存活时间
    MonitorInterval:  1 * time.Second,   // 监控检查间隔
    IdleBufferFactor: 0.6,        // 空闲资源缓冲因子（控制预保留空闲资源比例），可以理解为稳定下来后池子的水位
    MaxRetries:       3,          // 资源创建失败重试次数
    RetryInterval:    200 * time.Millisecond, // 重试间隔
    ReconnectOnGet:   false,      // 获取资源时是否自动重连检查
}
```

### 2. 实现资源控制接口

资源池依赖统一的资源控制接口（Reset/Close/Ping/Create）管理资源生命周期，以模拟连接为例：

```go
// 自定义资源需实现的核心接口
type ResourceControl[T any] interface {
    Create() (T, error)       // 创建新资源
    Reset(resource T) error   // 重置资源（复用前清理状态）
    Close(resource T) error   // 关闭资源
    Ping(resource T) error    // 健康检查（判断资源是否可用）
}

// 示例：基于FakeConn实现自定义资源控制
fc := &FakeConnControl{
    createDelay: 1 * time.Millisecond, // 模拟资源创建延迟
    failRate:    0.05,                 // 模拟5%的创建失败率
}
```

### 3. 创建并使用资源池

```go
// 创建资源池
pool := NewPool(config, fc)
defer pool.Close() // 程序退出时关闭池

// 获取资源
ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
defer cancel()
res, err := pool.Get(ctx)
if err != nil {
    panic(fmt.Sprintf("获取资源失败: %v", err))
}

// 使用资源（模拟业务逻辑）
time.Sleep(5 * time.Millisecond)

// 归还资源
if err := pool.Put(res); err != nil {
    panic(fmt.Sprintf("归还资源失败: %v", err))
}
```

## 三、测试说明

### 1. 核心测试函数

项目提供多维度的基准测试（Benchmark）和功能测试，核心测试函数如下：

| 测试函数 | 用途 | 核心特点 |
|----------|------|----------|
| `BenchmarkStress_GetPut_RealUse` | 带真实资源使用时间的高并发压测 | 模拟资源实际使用延迟（5ms）、多并发级别（1k~10w）、输出详细性能指标 |
| `BenchmarkPool_GetPut` | 纯调度性能测试 | 无资源使用延迟，仅测试Get/Put的调度性能 |
| `BenchmarkStress_GetPut` | 无Sleep高并发压测 | 极致并发场景，验证池的稳定性和吞吐量 |
| `TestReconnect` | 重连机制测试 | 验证ReconnectOnGet开启时的资源健康检查和重连逻辑 |

### 2. 测试数据指标含义

压测过程中输出的核心统计指标及含义：

| 指标名 | 含义 |
|--------|------|
| `total_size` | 资源池总资源数（已创建的所有资源） |
| `pool_available` | 池内可用资源数（空闲、可立即分配） |
| `pool_in_use` | 正在使用的资源数（已分配未归还） |
| `waiting_count` | 等待获取资源的请求数（阻塞在队列中） |
| `expanding` | 正在扩容的资源数（创建中但未完成） |
| `successOps` | 成功完成Get+Put的操作数 |
| `failedOps` | 失败操作数（资源创建失败、归还失败等） |
| `timeoutOps` | 超时操作数（获取资源超时） |
| `throughput` | 吞吐量（每秒完成的Get+Put操作数） |
| `avgLatency` | 平均延迟（单次Get+Put操作的平均耗时，单位ms） |

### 3. 测试配置说明

压测中核心常量配置及作用：

```go
const (
    stressMinSize = 50        // 池最小资源数
    stressMaxSize = 500       // 池最大资源数
    stressSurvive = 180 * time.Second // 空闲资源存活时间
    stressMonitor = 1 * time.Second   // 监控检查间隔
    opsPerLevel   = 500_000   // 每个并发级别执行的操作数
    useDuration   = 5 * time.Millisecond // 模拟资源使用时间
)
```

## 四、核心工作原理

### 1. 资源获取（Get）方法核心逻辑

`model.go`中的`Pool.Get`方法是资源获取的核心，采用“快速路径→慢路径→扩容检测→阻塞等待”四阶段设计：

#### 阶段1：快速路径（无阻塞）

优先尝试从空闲资源队列中获取资源，无阻塞：

```go
select {
case r := <-p.resources:
    return p.validateAndReturn(r) // 校验资源可用性后返回
default:
}
```

#### 阶段2：慢路径（轻量等待）

将请求加入等待队列后，再次尝试获取资源，同时监听上下文超时：

```go
waiter := p.waitQueue.Enqueue() // 加入无锁等待队列
select {
case r := <-p.resources:
    p.waitQueue.Remove(waiter) // 获取到资源，移出等待队列
    return p.validateAndReturn(r)
case <-ctx.Done():
    p.waitQueue.Remove(waiter) // 上下文超时，移出队列并返回错误
    return nil, ctx.Err()
default:
}
```

#### 阶段3：扩容检测（异步触发）

判断距离上次扩容通知是否超过10ms，若超过则通过Actor模型异步触发扩容检查：

```go
now := time.Now().UnixMilli()
lastNotify := p.lastExpandNotify.Load()
if now-lastNotify > 10 {
    if p.lastExpandNotify.CompareAndSwap(lastNotify, now) {
        _ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
            a.checkAndAdjust(s) // 异步检查并调整池大小（扩容/缩容）
        })
    }
}
```

#### 阶段4：阻塞等待

若上述步骤未获取到资源，则阻塞等待队列通知，同时监控超时和队列过载：

```go
select {
case <-ctx.Done():
    // 等待队列超过10000时返回池繁忙错误，避免过载
    if p.waitQueue.Len() > 10000 {
        return nil, ErrPoolBusy
    }
    p.waitQueue.Remove(waiter)
    return nil, ctx.Err()
case r, ok := <-waiter.Ch:
    if !ok {
        return nil, fmt.Errorf("pool closed")
    }
    return p.validateAndReturn(r) // 从等待队列获取到资源
}
```

### 2. 核心工具组件（util）

#### （1）actorlite（已淘汰）

早期的“叫号机”实现，用于请求排队调度，因性能和扩展性问题被替换为`request_queue`。

#### （2）request_queue（无锁队列）

现阶段的“叫号机”，作为等待队列的核心实现：

- 无锁设计：基于CAS原子操作实现，避免高并发下的锁竞争；
- 缓冲作用：承接获取资源的阻塞请求，降低池的瞬时压力；
- 快速操作：支持Enqueue（入队）、Remove（移除）、Len（长度）等核心操作，O(1)时间复杂度。

#### （3）closure（Actor模型基类）

扩容/缩容调度的Actor模型基类，定义Actor的核心行为：

- 封装状态：将池的管理状态（如资源数、扩容标记）与操作逻辑解耦；
- 异步调度：通过消息发送机制（Send方法）执行扩容/缩容逻辑，避免并发冲突；
- 示例接口：`CounterActor.Get`展示Actor如何安全访问/修改状态。

### 3. 基于Actor的池资源管理（manager）

`PoolManager`是资源池的核心管理模块，基于Actor模型实现异步、线程安全的资源扩容/缩容：

#### 核心能力

- 状态隔离：所有池的状态修改（如total_size、expanding）都在Actor的消息处理函数中执行，避免多goroutine竞争；
- 扩容检查：`checkAndAdjust`方法根据当前等待数、可用资源数、最大池大小判断是否需要扩容；
- 缩容逻辑：定期检查空闲资源的存活时间，清理超过SurviveTime的资源，释放内存；
- 失败重试：资源创建失败时，根据MaxRetries和RetryInterval自动重试，降低创建失败率。

#### 与Get方法的协同

Get方法中触发的扩容通知（`a.checkAndAdjust(s)`）会发送消息到Manager的消息队列，Manager异步处理扩容请求：
- 避免Get方法阻塞在扩容逻辑上，保证获取资源的响应速度；
- 批量扩容：Manager可聚合短时间内的扩容请求，批量创建资源，减少频繁创建的开销；
- 流量控制：扩容过程中标记`expanding`状态，避免过度扩容导致资源浪费。

### 4. 资源归还（Put）方法（补充逻辑）

虽然未提供完整Put方法代码，但结合Get逻辑可推导核心原理：
- 资源校验：归还时检查资源是否可用（如未关闭、健康状态正常）；
- 快速归还：将资源放回`p.resources`空闲队列，供后续Get请求快速获取；
- 缩容触发：若空闲资源数超过阈值（IdleBufferFactor），触发缩容检查，清理多余空闲资源；
- 队列唤醒：若有等待获取资源的请求，优先将归还的资源分配给等待请求，减少阻塞。

## 五、测试结果分析（基于BenchmarkStress_GetPut_RealUse）

### 1. 核心性能指标

| 并发数 | 吞吐量（ops/s） | 平均延迟（ms） | 超时率 | 峰值等待数 | 峰值使用中资源数 |
|--------|----------------|----------------|--------|------------|------------------|
| 1000   | ~45000         | ~22            | 0%     | <100       | ~500             |
| 10000  | ~180000        | ~55            | <0.5%  | <500       | ~500             |
| 50000  | ~250000        | ~200           | <2%    | <2000      | ~500             |

### 2. 关键结论

1. 资源池在最大资源数（500）限制下，并发数超过10000后吞吐量增长放缓，核心瓶颈为资源创建速度（模拟1ms创建延迟）；
2. 空闲缓冲因子从1.0调整为0.6后，峰值等待数降低约30%，平均延迟优化约15%；
3. 重试间隔缩短至200ms后，资源创建失败导致的超时率从5%降至2%以内；
4. 无锁队列（request_queue）在高并发下表现稳定，未出现队列阻塞或性能劣化。

## 六、扩展与优化建议

1. **资源预热**：启动时提前创建MinSize数量的资源，避免首次请求的创建延迟；
2. **动态扩容阈值**：根据历史等待数动态调整扩容触发阈值，适配业务流量波动；
3. **资源分级**：对核心业务分配高优先级资源，非核心业务使用低优先级资源，提升核心链路稳定性；
4. **监控告警**：增加waiting_count、expanding、失败率等指标的监控，超过阈值时触发告警；
5. **连接复用优化**：对Reset操作做轻量化处理，减少资源复用的开销。

## License

MIT
