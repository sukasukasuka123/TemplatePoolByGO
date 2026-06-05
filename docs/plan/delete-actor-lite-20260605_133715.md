# P1-2: 删除 actor_lite 死代码

**时间**: 2026-06-05 21:37 UTC
**状态**: ✅ 已修复
**文件**: util/actor_lite/actor_lite.go（删除）

## 问题描述

`util/actor_lite/actor_lite.go` 定义了 `ActorLite[T]` 结构体和 `Do()` 方法，提供了极简的 Actor 实现（27 行）。但整个项目**零引用**。项目实际使用的是 `Closure`（285 行）。

保留它没有任何好处，只会：
1. 增加代码库体积
2. 误导新贡献者（不知道用哪个 Actor 实现）

## 修复方案

直接删除 `util/actor_lite/` 目录。

关于是否用 actor_lite 替换 Closure：池子对 Actor 的使用极其单一（只调 Send，fire-and-forget），actor_lite 确实更合适。但 Closure 有成熟的测试覆盖（8 个测试，包含并发/竞争），替换的风险大于收益。**先删除死代码，保持 Closure**。

## 影响范围

- 删除 `util/actor_lite/actor_lite.go`

## 冲突检查

- 无冲突：死代码，零引用
