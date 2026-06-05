# P3-1: doPingRound 圈复杂度过高

**时间**: 2026-06-05 21:37 UTC
**状态**: ✅ 已修复
**文件**: model.go:86-152

## 问题描述

`doPingRound` 使用 `goto`、三层嵌套 `select`、"放回后再抢出来检查"的二次检查逻辑。圈复杂度约 15+，出 bug 时很难定位。

## 修复方案

拆分为两个函数：

1. `collectPingBatch(n int) []*resource[T]` — 从 resources channel 取最多 n 个连接，中途有人排队则放回已取的
2. `processPingBatch(batch []*resource[T])` — 对每连接 Ping → 健康则放回 + 二次检查 → 不健康则 Close + 通知 Actor

```go
func (p *Pool[T]) collectPingBatch(maxCount int) []*resource[T] {
    batch := make([]*resource[T], 0, maxCount)
    for i := 0; i < maxCount; i++ {
        if p.waitQueue.Len() > 0 {
            p.returnBatch(batch) // 有等待者，放回
            return nil
        }
        select {
        case r := <-p.resources:
            batch = append(batch, r)
        default:
            return batch // channel 空了，开始处理
        }
    }
    return batch
}

func (p *Pool[T]) processPingBatch(batch []*resource[T]) {
    for _, r := range batch {
        if err := p.connControl.Ping(r.Conn); err != nil {
            // 不健康：异步通知 Actor 处理
            _ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
                a.connControl.Close(r.Conn)
                a.poolTotalSize.Add(-1)
                a.checkAndAdjust(s)
            })
            if p.config.OnUnhealthy != nil {
                p.config.OnUnhealthy(err)
            }
        } else {
            // 健康：优先给等待者，否则放回
            if p.waitQueue.TryDequeue(r) {
                continue
            }
            p.tryReturnResource(r)
        }
    }
}

func (p *Pool[T]) tryReturnResource(r *resource[T]) {
    select {
    case p.resources <- r:
        // 放回后二次检查
        if p.waitQueue.Len() > 0 {
            select {
            case r2 := <-p.resources:
                if !p.waitQueue.TryDequeue(r2) {
                    select {
                    case p.resources <- r2:
                    default:
                        p.connControl.Close(r2.Conn)
                        p.totalSize.Add(-1)
                    }
                }
            default:
            }
        }
    default:
        p.connControl.Close(r.Conn)
        p.totalSize.Add(-1)
    }
}
```

## 影响范围

- model.go: doPingRound 拆分为 3 个辅助函数
- 行为完全等价

## 冲突检查

- 与 P2-2（validateAndReturn）无冲突：Ping 逻辑在心跳路径，不在 Get 路径
