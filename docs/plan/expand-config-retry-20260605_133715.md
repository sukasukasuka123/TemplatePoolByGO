# P0-1: Expand 忽略配置的 MaxRetries 和 RetryInterval

**时间**: 2026-06-05 21:37 UTC
**状态**: ✅ 已修复
**文件**: pool_manager.go:186-195

## 问题描述

`expand()` 方法中创建连接的重试逻辑硬编码了 `retry < 3` 和 `time.Sleep(5 * time.Millisecond)`，
完全忽略 `PoolConfig` 中用户配置的 `MaxRetries` 和 `RetryInterval`。

用户即使配置 `MaxRetries: 10, RetryInterval: 2s`，扩容时仍然只重试 3 次，间隔 5ms。

## 修复方案

1. 使用 `s.config.MaxRetries` 替代硬编码的 `3`
2. 使用 `s.config.RetryInterval` 替代硬编码的 `5 * time.Millisecond`
3. 添加 `maxRetries < 1` 的下限保护（防止配置 0 导致连接零值泄漏）
4. 最后一次重试后不 sleep（避免无效等待）

## 影响范围

- pool_manager.go: expand() 方法
- 无破坏性变更，完全向后兼容（默认值 MaxRetries=3, RetryInterval=1s）

## 验证

- `go build ./...` ✅
- `go vet ./...` ✅
- race 检测 ✅
- 单元测试全部通过 ✅
