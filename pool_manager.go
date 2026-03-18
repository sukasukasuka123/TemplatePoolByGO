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
// ===== 核心修复：扩容逻辑不应该依赖 buffer 的 idleRatio =====
func (a *PoolManagerActor[T]) checkAndAdjust(s *PoolManagerState[T]) {
	if !a.initialized.Load() {
		return
	}

	poolLen := int64(len(a.sharedResources))
	capacity := int64(cap(a.sharedResources))
	waiting := int64(a.waitQueue.Len())
	currentTotal := a.poolTotalSize.Load()
	expandingCount := a.expanding.Load()
	effectiveTotal := currentTotal + expandingCount

	// ===== 正确的扩容逻辑 =====
	// 不要用 idleRatio，而是直接看：
	// 1. 是否有人在等（有需求）
	// 2. 总连接数是否达到上限（有空间）

	shouldExpand := false

	// 条件 1：有人在排队（最高优先级）
	if waiting > 0 && effectiveTotal < s.config.MaxSize {
		shouldExpand = true
	}

	// 条件 2：空闲连接不足（基于 buffer 利用率）
	// 但这里的判断应该是：如果 buffer 快满了，说明空闲连接很多，不需要扩容
	// 如果 buffer 很空，说明连接都在用，可能需要扩容
	if !shouldExpand && effectiveTotal < s.config.MaxSize {
		// 关键修改：只有当 buffer 利用率低（空闲少）且还没到 MaxSize 时才扩容
		bufferUtilization := float64(poolLen) / float64(capacity)
		if bufferUtilization < 0.3 { // buffer 利用率低于 30%，说明大部分连接都在用
			shouldExpand = true
		}
	}

	if shouldExpand {
		expandSize := a.calculateExpandSize(s, waiting, effectiveTotal)
		if expandSize > 0 {
			a.expand(s, expandSize)
		}
		return
	}

	// ===== 缩容逻辑 =====
	// 只有当空闲连接很多（buffer 快满）且没人等待时才缩容
	bufferUtilization := float64(poolLen) / float64(capacity)
	if bufferUtilization > 0.9 && waiting == 0 && expandingCount == 0 && currentTotal > s.config.MinSize {
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
	// 取等待人数的 1/3 作为压力补偿
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
	limit := maxSize / 3
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

func (a *PoolManagerActor[T]) expand(s *PoolManagerState[T], expandSize int64) {
	if a.poolTotalSize.Load()+a.expanding.Load() >= s.config.MaxSize {
		return
	}

	for i := int64(0); i < expandSize; i++ {
		newExpanding := a.expanding.Add(1)
		newTotal := a.poolTotalSize.Load() + newExpanding

		if newTotal > s.config.MaxSize {
			a.expanding.Add(-1)
			break
		}

		go func(idx int64) {

			var conn T
			var err error
			for retry := 0; retry < 3; retry++ {
				conn, err = a.connControl.Create()
				if err == nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err != nil {
				a.expanding.Add(-1)
				return
			}

			_ = a.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
				a.expanding.Add(-1)
				a.poolTotalSize.Add(1)
				res := &resource[T]{
					ID:         fmt.Sprintf("exp-%d-%d", time.Now().UnixNano(), idx),
					createTime: time.Now(),
					updateTime: time.Now(),
					Conn:       conn,
				}
				if a.waitQueue.TryDequeue(res) {
					return
				}
				select {
				case a.sharedResources <- res:
				default:
					a.connControl.Close(conn)
					a.poolTotalSize.Add(-1)
				}
			})
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
