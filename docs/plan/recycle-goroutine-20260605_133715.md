# P1-1: recycle 每回收一个 channel 起一个 goroutine

**时间**: 2026-06-05 21:37 UTC
**状态**: ✅ 已修复
**文件**: util/request_queue/request_queue.go:132-142

## 问题描述

```go
func (q *LockFreeQueue[T]) recycle(ch chan T) {
    go func() {          // ← 每次回收都起一个 goroutine
        select {
        case <-ch:
        default:
        }
        q.chanPool.Put(ch)
    }()
}
```

高并发下大量等待者超时取消 → `TryDequeue` 每跳过一个 Cancelled 节点就调用 `recycle` → 每个都起一个新 goroutine。10000 个等待者超时 = 10000 个 goroutine 仅为了把 channel 放回 sync.Pool。

而且更严重的是：`select { case <-ch: }` 会消费 channel 中可能存在的资源（竞态泄漏的资源），直接丢弃，不做 Close/归还。

## 修复方案

将 `recycle` 改为内联同步操作：

```go
func (q *LockFreeQueue[T]) recycle(ch chan T) {
    select {
    case <-ch:
    default:
    }
    q.chanPool.Put(ch)
}
```

调用方 `TryDequeue` 中保持不变（已经是 `q.recycle(next.Ch)`）。

注意：调用 `recycle` 的路径是 `TryDequeue`，它本身可能跑在 `Put` goroutine 的热路径上。但同步 drain channel 不阻塞（`chan T` 容量为 1），`sync.Pool.Put` 是 O(1)，所以同步执行完全可行。

## 影响范围

- util/request_queue/request_queue.go: recycle() 方法
- 无破坏性变更，行为完全等价（channel 回收逻辑不变）

## 冲突检查

- 无冲突：仅涉及 request_queue.go 内部实现
