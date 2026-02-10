package pool

import (
	"fmt"
	"sync/atomic"
	"time"

	closure "github.com/sukasukasuka123/TemplatePoolByGO/util/Closure"
	"github.com/sukasukasuka123/TemplatePoolByGO/util/request_queue"
)

type PoolManagerState[T any] struct {
	config      PoolConfig
	connControl Conn[T]
}

type PoolManagerActor[T any] struct {
	closure.BaseActor[PoolManagerState[T]]
	config          PoolConfig
	connControl     Conn[T]
	manager         *closure.Closure[PoolManagerState[T], *PoolManagerActor[T]]
	sharedResources chan *resource[T]
	waitQueue       *request_queue.LockFreeQueue[*resource[T]]
	poolTotalSize   *atomic.Int64
	expanding       *atomic.Int64 // 新增：记录扩容中的连接数
	initialized     atomic.Bool
}

func NewPoolManagerActor[T any](
	config PoolConfig,
	connControl Conn[T],
	totalSize *atomic.Int64,
	wq *request_queue.LockFreeQueue[*resource[T]],
	expanding *atomic.Int64,
) *PoolManagerActor[T] {
	return &PoolManagerActor[T]{
		config:        config,
		connControl:   connControl,
		poolTotalSize: totalSize,
		waitQueue:     wq,
		expanding:     expanding,
	}
}

func (a *PoolManagerActor[T]) Init() PoolManagerState[T] {
	return PoolManagerState[T]{
		config:      a.config,
		connControl: a.connControl,
	}
}

// checkAndAdjust 检测并调整池大小（更敏感的扩容触发条件）
func (a *PoolManagerActor[T]) checkAndAdjust(s *PoolManagerState[T]) {
	if !a.initialized.Load() {
		return
	}

	poolLen := int64(len(a.sharedResources))
	capacity := int64(cap(a.sharedResources))
	waiting := int64(a.waitQueue.Len())
	currentTotal := a.poolTotalSize.Load()
	expandingCount := a.expanding.Load()

	// ============ 扩容逻辑（更敏感的触发条件）============

	// 条件 A: 有人在排队（最高优先级）
	// 条件 B: 空闲率低于 70%（原来是 65%，现在提高阈值更敏感）
	// 条件 C: 考虑正在扩容中的连接数

	idleRatio := float64(poolLen) / float64(capacity)
	effectiveTotal := currentTotal + expandingCount // 包含正在创建的连接

	shouldExpand := (waiting > 0 || idleRatio < 0.70) && effectiveTotal < s.config.MaxSize

	if shouldExpand {
		expandSize := a.calculateExpandSize(s, waiting, effectiveTotal)
		if expandSize > 0 {
			a.expand(s, expandSize)
		}
		return // 触发扩容后直接返回，不执行缩容检查
	}

	// ============ 缩容逻辑 ============
	// 空闲很多（> 85%，提高阈值避免频繁缩容），且完全没人在等，且没有正在扩容的
	if idleRatio > 0.85 && waiting == 0 && expandingCount == 0 && currentTotal > s.config.MinSize {
		a.shrink(s)
	}
}

// calculateExpandSize 计算扩容大小（优化的非线性曲线 + 压力补偿）
func (a *PoolManagerActor[T]) calculateExpandSize(s *PoolManagerState[T], waiting int64, effectiveTotal int64) int64 {
	maxSize := s.config.MaxSize
	minSize := s.config.MinSize

	if effectiveTotal >= maxSize {
		return 0
	}

	// ============ 非线性曲线逻辑 ============
	usedRange := effectiveTotal - minSize
	totalRange := maxSize - minSize
	if totalRange <= 0 {
		totalRange = 1
	}
	usageRate := float64(usedRange) / float64(totalRange)

	var baseStep int64
	switch {
	case usageRate < 0.20: // 起步期：保守扩容
		baseStep = 15
	case usageRate < 0.75: // 爆发期：激进扩容
		remaining := maxSize - effectiveTotal
		baseStep = remaining / 2 // 原来是 /3，现在更激进
	default: // 收敛期：谨慎扩容
		remaining := maxSize - effectiveTotal
		baseStep = remaining / 8 // 原来是 /10
	}

	// ============ 压力补偿逻辑 ============
	// 如果排队人数很多，baseStep 可能跟不上
	// 取等待人数的 1/3 作为压力补偿（原来是 1/4，现在更激进）
	pressureStep := waiting / 3
	if waiting > 100 { // 如果等待人数超过100，进一步加速
		pressureStep = waiting / 2
	}

	// 最终步长 = max(曲线步长, 压力补偿)
	step := baseStep
	if pressureStep > step {
		step = pressureStep
	}

	// ============ 限制和保护 ============
	// 单次扩容不宜超过 MaxSize 的 25%（原来是 20%，现在允许更大步长）
	limit := maxSize / 4
	if step > limit {
		step = limit
	}

	// 保护：不超过剩余空间
	if effectiveTotal+step > maxSize {
		step = maxSize - effectiveTotal
	}

	// 至少扩容 1 个
	if step < 1 && effectiveTotal < maxSize {
		step = 1
	}

	return step
}

// expand 异步扩容（高并发优化）
func (a *PoolManagerActor[T]) expand(s *PoolManagerState[T], expandSize int64) {
	// 二次校验
	if a.poolTotalSize.Load()+a.expanding.Load() >= s.config.MaxSize {
		return
	}

	// ============ 高并发扩容优化 ============
	// 信号量并发数：200（原来是100，现在提高并发度）
	// 这意味着可以同时创建200个连接，大幅提升扩容速度
	sem := make(chan struct{}, 200)

	for i := int64(0); i < expandSize; i++ {
		// 预先占位，避免重复扩容
		newExpanding := a.expanding.Add(1)
		newTotal := a.poolTotalSize.Load() + newExpanding

		if newTotal > s.config.MaxSize {
			a.expanding.Add(-1) // 回退
			break
		}

		sem <- struct{}{}
		go func(idx int64) {
			defer func() {
				<-sem
				a.expanding.Add(-1) // 扩容完成，减少计数
			}()

			var conn T
			var err error

			// 重试逻辑：最多3次，间隔缩短
			for retry := 0; retry < 3; retry++ {
				conn, err = a.connControl.Create()
				if err == nil {
					break
				}
				// 压测时重试间隔缩短
				time.Sleep(10 * time.Millisecond)
			}

			if err != nil {
				// 创建失败，不增加 totalSize
				return
			}

			// 成功创建连接
			a.poolTotalSize.Add(1)

			res := &resource[T]{
				ID:         fmt.Sprintf("exp-%d-%d", time.Now().UnixNano(), idx),
				createTime: time.Now(),
				updateTime: time.Now(),
				Conn:       conn,
			}

			// ============ 资源分发策略 ============
			// 1. 优先送给正在排队的 Get 请求（资源直达）
			if a.waitQueue.TryDequeue(res) {
				return
			}

			// 2. 没人要，放回共享池
			select {
			case a.sharedResources <- res:
			default:
				// 3. 池子满了（可能并发竞争或缩容），关闭连接
				a.connControl.Close(conn)
				a.poolTotalSize.Add(-1)
			}
		}(i)
	}
}

// shrink 缩容逻辑
func (a *PoolManagerActor[T]) shrink(s *PoolManagerState[T]) {
	currentTotal := a.poolTotalSize.Load()
	target := currentTotal - s.config.MinSize
	if target <= 0 {
		return
	}

	// 渐进式缩容：每次最多缩 20%
	shrinkSize := target / 5
	if shrinkSize < 1 {
		shrinkSize = 1
	}
	if shrinkSize > target {
		shrinkSize = target
	}

	closedCount := int64(0)
	for closedCount < shrinkSize {
		select {
		case r := <-a.sharedResources:
			a.connControl.Close(r.Conn)
			a.poolTotalSize.Add(-1)
			closedCount++
		default:
			return // 没有更多可关闭的了
		}
	}
}
