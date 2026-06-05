# P0-2: Get 里的资源泄漏窗口

**时间**: 2026-06-05 21:37 UTC
**状态**: ✅ 已修复
**文件**: model.go:185-205

## 问题描述

`Get()` 方法中存在 Enqueue → TryDequeue 竞态导致的资源泄漏：

```
时间线 A (Get goroutine)          时间线 B (Put goroutine)
──────────────────────────────────────────────────────────
waiter := Enqueue()              
                                  TryDequeue(res) 匹配到 waiter
                                  → res 投递到 waiter.Ch ✓
select { case r := <-p.resources:
    Remove(waiter)    // Cancelled=true
    return r           // ← waiter.Ch 中的 res 永久丢失
}
```

## 修复方案

在快速路径 (`p.resources`) 成功获取资源后，用非阻塞 `select` 排空 `waiter.Ch`：

1. 若 channel 中有资源（竞态已发生），取出
2. 优先 TryDequeue 给下一个等待者
3. 其次放回 `p.resources` channel
4. 最后才 Close（channel 满无法放回时）

若竞态未发生，`default` 分支直接跳过，零开销。

## 影响范围

- model.go: Get() 方法，快速路径分支
- 无破坏性变更，不改变正常路径行为

## 验证

- `go build ./...` ✅
- `go vet ./...` ✅
- race 检测 ✅
- 单元测试全部通过 ✅
- 压力测试 50/100/200/500 并发全部通过 ✅
