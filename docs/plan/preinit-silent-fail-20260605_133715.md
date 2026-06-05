# P3-2: preInit 静默吞掉创建失败

**时间**: 2026-06-05 21:37 UTC
**状态**: ✅ 已修复
**文件**: model.go:154-171

## 问题描述

```go
for i := int64(0); i < count; i++ {
    conn, err := cc.Create()
    if err != nil {
        continue  // 静默跳过，无日志，无错误返回
    }
    ...
}
```

如果 MinSize=5，5 次全部失败，池子以 totalSize=0 静默启动，initialized 照样设为 true，第一个 Get 调用方要承受完整扩容延迟。

## 修复方案

添加日志记录和启动后状态检查：

```go
func (p *Pool[T]) preInit(count int64, cc Conn[T]) {
    var failed int64
    for i := int64(0); i < count; i++ {
        conn, err := cc.Create()
        if err != nil {
            failed++
            log.Printf("[TemplatePoolByGO] preInit: failed to create connection %d/%d: %v", i+1, count, err)
            continue
        }
        p.totalSize.Add(1)
        p.resources <- &resource[T]{
            ID:         fmt.Sprintf("init-%d", i),
            createTime: time.Now(),
            updateTime: time.Now(),
            Conn:       conn,
        }
    }
    created := count - failed
    if failed > 0 {
        log.Printf("[TemplatePoolByGO] preInit: %d/%d connections created successfully, %d failed", created, count, failed)
    }
    _ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
        a.initialized.Store(true)
    })
}
```

## 影响范围

- model.go: preInit() 方法
- 新增 import "log"

## 冲突检查

- 无冲突：纯日志增强，不改变行为
