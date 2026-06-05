# 锐评 TemplatePoolByGO

## 做得好的

1. Actor 隔离扩缩容决策 —— 确实是最正确的架构选择

把扩缩容从 Get 热路径里拿出去，用 CAS + 10ms 时间窗口限流，所有决策串行跑在一个 eventLoop 里。这个设计在 Java 的连接池里是标配（HikariCP 的 HouseKeeper），但在 Go 生态里大多数人还在 Get 里加锁直接扩容。你这条路走对了。而且扩容曲线三段式（保守→爆发→收敛）+ 压力补偿（max(曲线步长, 等待人数/3)）比大多数池子的线性扩容聪明得多。

2. 无锁等待队列 —— Michael-Scott 变体用得对

每个等待者独立 chan T，取消只标记 + 懒删除，归还时点对点交付。避免了"所有人堵在同一个 channel 上"的惊群问题，也避免了从链表中间摘除节点的复杂并发。sync.Pool 复用 waiter channel 也是对的。

3. 心跳不抢业务连接 —— 这个细节很多人会漏

doPingRound 取连接前检查等待队列，取到一半发现有新人来了就把已取出的全放回去。大多数连接池的心跳和业务是各跑各的，高负载时心跳反而在跟业务抢连接，你把这个 case 处理对了。

4. 泛型用得干净

Conn[T] 接口四个方法，池子逻辑完全不碰具体类型。换一种连接只需换一个实现。比 interface{} + 类型断言的方式安全太多。

5. README 写得好

把"为什么"（问题→方案→取舍）讲清楚了，不是那种"这是配置项 A，这是配置项 B"的 API 文档。IdleBufferFactor 专门开一节解释，因为这个字段确实容易误解。

## 有问题的

1. Expand 忽略了配置里的重试参数 —— 这是一个 bug

pool_manager.go:186-195 硬编码了 for retry := 0; retry < 3; retry++ 和 time.Sleep(5 * time.Millisecond)，但 PoolConfig 里有 MaxRetries 和 RetryInterval 两个字段，配置了根本不生效：

// pool_manager.go:186 —— 硬编码，无视 config.MaxRetries 和 config.RetryInterval
for retry := 0; retry < 3; retry++ {
    conn, err = a.connControl.Create()
    if err == nil {
        break
    }
    time.Sleep(5 * time.Millisecond)
}
应该是 for retry := 0; retry < s.config.MaxRetries; retry++ 和 time.Sleep(s.config.RetryInterval)。用户配了 MaxRetries: 10, RetryInterval: 2s，扩容时还是只重试 3 次、间隔 5ms。

2. validateAndReturn 根本没做验证 —— 名不副实

model.go:248-252：

func (p *Pool[T]) validateAndReturn(r *resource[T]) (*resource[T], error) {
    p.inUse.Add(1)
    r.updateTime = time.Now()
    return r, nil
}
既不验证连接是否存活，也不检查 ReconnectOnGet 配置。ReconnectOnGet 这个字段存在于配置中，README 里写了"Get 时 Reset 失败是否自动重连"，实际上 Get 里从来没调过 Reset。Reset 只在 Put 时调用。所以 ReconnectOnGet=true 的实际行为是"归还时 Reset 失败就关掉连接"，跟"Get 时自动重连"是两码事。

要么把 ReconnectOnGet 的逻辑真正实现在 validateAndReturn 里（Get 时 Ping/Reset，失败则重连），要么把配置名和文档改对。

3. Get 里存在资源泄漏窗口

model.go:185-190：

waiter := p.waitQueue.Enqueue()

select {
case r := <-p.resources:   // 运气好，插队拿到了
    p.waitQueue.Remove(waiter)  // 从队列里删掉自己
    return p.validateAndReturn(r)
default:
}
这里有个 race：Enqueue 之后、Remove 之前，另一个 goroutine 的 TryDequeue 可能已经匹配到了这个 waiter，往 waiter.Ch 里塞了一个资源。然后 Remove 只标记了 Cancelled=true，TryDequeue 那边已经投递成功了。但 Get 走了 <-p.resources 的路径，永远不会去读 waiter.Ch。那个被投递到 waiter.Ch 里的资源就永远丢失了（goroutine 泄漏 + 连接泄漏）。

虽然不是高频触发（需要精确的时序重叠），但它存在。

4. recycle 每回收一个 channel 就起一个 goroutine

request_queue.go:132-142：

func (q *LockFreeQueue[T]) recycle(ch chan T) {
    go func() {          // <-- 每次回收都起一个 goroutine
        select {
        case <-ch:
        default:
        }
        q.chanPool.Put(ch)
    }()
}
高并发下等待者大量超时取消时，TryDequeue 里每路过一个 Cancelled 节点就调用 recycle，每次起一个新 goroutine。10000 个等待者超时 = 10000 个 goroutine 只是为了把 channel 放回 sync.Pool。完全可以用 ring buffer 或者直接内联 select { case <-ch: default: }; q.chanPool.Put(ch)。不需要异步。

5. SurviveTime 是幽灵配置

配置里有 SurviveTime: 30 * time.Minute，README 里写了"连接最长存活时间"，resource 结构体里记了 createTime 和 updateTime。但整个代码库里没有任何地方读取这两个时间字段来做驱逐判断。shrink 只看 channel 利用率，不看连接年龄。一条连接可以活到进程退出。

要么实现它（在 shrink 或心跳里检查连接年龄），要么从配置和文档里删掉。

6. actor_lite 是完全的死代码

util/actor_lite/actor_lite.go 定义了另一套 Actor 实现，但整个项目没有任何地方 import 它。它比 Closure 更简洁，但实际上池子用的是 Closure。要么删掉，要么用它替换 Closure（因为池子只需要 Send，不需要 Call/CallTyped/CallWithContext 这些复杂同步机制）。

7. Closure Actor 对池子的场景来说过度设计了

池子对 Actor 的使用方式极其单一：只调 Send（fire-and-forget）。但 Closure 提供了 Call、CallWithContext、CallTyped、Send、TrySend、GetState、GetActor 一共 7 个方法，两个泛型参数 T 和 A，还有 reply/errCh 双向通信通道、panic recover。这些池子一条都不需要。Send 本质上就是一个 chan func() + 单 goroutine 消费，50 行代码就能搞定。Closure 写了 285 行。

而且 Closure.Send 在 inbox 满的时候会阻塞（没有 default 分支），model.go:197 的 manager.Send 跑在 Get 热路径上。虽然 inbox size 设了 1000 不太可能满，但万一满了，Get 就会卡住。

8. doPingRound 的控制流过于复杂

model.go:86-152 用了 goto ping、嵌套的三层 select、放回后再抢出来检查的"二次检查"逻辑。这个函数的圈复杂度大概在 15+，而且处理的是并发最微妙的场景（心跳 vs 业务 vs 等待队列）。出 bug 时很难定位。至少应该拆成 collectBatch + processBatch 两个函数，把"取一批连接"和"Ping 并放回"的职责分开。

9. preInit 静默吞掉创建失败

model.go:154-171：

for i := int64(0); i < count; i++ {
    conn, err := cc.Create()
    if err != nil {
        continue  // 静默跳过，不记日志，不返回错误
    }
    ...
}
如果 MinSize=5，5 次全部失败，池子以 totalSize=0 静默启动，initialized 照样设为 true，第一个 Get 调用方要吃一个完整的扩容延迟。至少应该记个 log，或者返回一个 error（比如把 NewPool 的返回值改成 (*Pool[T], error)）。

10. Stats 的 pool_in_use 统计有时序偏差

inUse 在 Put 里是先递减再归还资源：

// Put 里
p.inUse.Add(-1)     // 先减
p.resources <- res  // 后归还
这导致 Stats 读数会出现 pool_in_use + pool_available < total_size 的情况（差值就是"正在归还中"的连接）。README 里写了"近似，有极短的中间态"，但这是一个可以修的问题——把 Add(-1) 移到 <-p.resources 成功之后就行。

总评
维度    评价
架构设计    ★★★★☆ Actor 隔离扩缩容 + 无锁队列是正确的方向，但 Closure Actor 过度设计
并发安全    ★★★☆☆ 主体路径正确，但 Get 里有资源泄漏窗口，Stats 有统计偏差
代码质量    ★★★☆☆ 核心逻辑清晰但 doPingRound 太复杂，preInit 静默吞错，死代码没清理
配置完整性    ★★☆☆☆ SurviveTime 没实现，ReconnectOnGet 行为与文档不符，Expand 忽略重试配置
文档    ★★★★★ README 是亮点，把"为什么这么做"讲清楚了
可用性    ★★★★☆ 接入成本低，泛型接口清晰，go get 即用
一句话：核心思路对，Actor + 无锁队列 + 三阶段扩容的设计比绝大多数 Go 连接池讲究，但实现上有几个配置不生效的 bug 和一些并发边角 case，修掉之后是一个可以正经上生产的库。

如果要修，优先级排序
P0 — Expand 使用 config.MaxRetries 和 config.RetryInterval（现在硬编码，配置不生效）
P0 — Get 里的资源泄漏窗口（Enqueue → TryDequeue 竞态 → 连接丢失）
P1 — recycle 去掉 goroutine 起爆，改成内联
P1 — 删掉 actor_lite 死代码，或者用它替换 Closure 简化依赖
P2 — 实现 SurviveTime 或在文档里标注"未实现"
P2 — validateAndReturn 要么真做验证，要么改名，ReconnectOnGet 行为对齐文档
P3 — 拆分 doPingRound，降圈复杂度
P3 — preInit 失败时打日志或返回 error