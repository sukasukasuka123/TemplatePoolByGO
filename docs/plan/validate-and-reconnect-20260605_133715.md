# P2-2: validateAndReturn 名不副实 + ReconnectOnGet 文档不符

**时间**: 2026-06-05 21:37 UTC
**状态**: ✅ 已修复
**文件**: model.go, config.go, README.md

## 问题描述

1. `validateAndReturn` 方法名暗示验证，但实际只做 `inUse.Add(1)` + `updateTime = time.Now()`，零验证
2. `ReconnectOnGet` 配置字段文档写"Get 时 Reset 失败是否自动重连"，但 Get 里从不调 Reset。实际 Reset 只在 Put 时调用

## 修复方案

**方案**：将 `ReconnectOnGet` 行为正确实现在 `validateAndReturn` 中：

```go
func (p *Pool[T]) validateAndReturn(r *resource[T]) (*resource[T], error) {
    // 如果配置了 Get 时验证，则 Ping 检测连接存活
    if p.config.ReconnectOnGet {
        if err := p.connControl.Ping(r.Conn); err != nil {
            // Ping 失败，尝试重连
            for retry := 0; retry < p.config.MaxRetries; retry++ {
                newConn, createErr := p.connControl.Create()
                if createErr == nil {
                    p.connControl.Close(r.Conn)
                    r.Conn = newConn
                    r.createTime = time.Now()
                    r.retryCount++
                    break
                }
                if retry < p.config.MaxRetries-1 {
                    time.Sleep(p.config.RetryInterval)
                }
            }
        }
    }
    p.inUse.Add(1)
    r.updateTime = time.Now()
    return r, nil
}
```

**重要**：`ReconnectOnGet` 默认值为 `true`（在 DefaultPoolConfig 中）。这意味着默认行为会改变——原来不验证，修复后每次 Get 都会 Ping。这可能有性能影响。

**折中方案**：将默认值改为 `false`，让用户显式开启。这样不改现有默认行为，但文档和实际行为对齐。同时将方法名改为 `prepareAndReturn` 或保持 `validateAndReturn`（现在名副其实了）。

## 影响范围

- model.go: validateAndReturn() — 真正实现验证+重连
- config.go: DefaultPoolConfig 中 ReconnectOnGet 默认值改为 false
- README.md: 更新 ReconnectOnGet 描述（对齐实际行为）

## 冲突检查

- 与现有 Get 流程兼容（原返回值不变）
- 与 P2-1（SurviveTime）无冲突
- 性能：默认 ReconnectOnGet=false，不影响现有用户
