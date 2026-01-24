// ========== pool_manager.go ==========
package pool

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	closure "github.com/sukasukasuka123/TemplatePoolByGO/util/Closure"
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
	poolTotalSize   *atomic.Int64
	initialized     atomic.Bool
}

func NewPoolManagerActor[T any](config PoolConfig, connControl Conn[T], totalSize *atomic.Int64) *PoolManagerActor[T] {
	return &PoolManagerActor[T]{
		config:        config,
		connControl:   connControl,
		poolTotalSize: totalSize,
	}
}

func (a *PoolManagerActor[T]) Init() PoolManagerState[T] {
	return PoolManagerState[T]{
		config:      a.config,
		connControl: a.connControl,
	}
}

// 非线性扩容算法
func (a *PoolManagerActor[T]) calculateExpandSize(s *PoolManagerState[T]) int64 {
	currentSize := a.poolTotalSize.Load()
	maxSize := s.config.MaxSize
	minSize := s.config.MinSize
	totalRange := maxSize - minSize

	if totalRange <= 0 {
		return 0
	}

	// 当前已使用的范围比例
	usageRate := float64(currentSize-minSize) / float64(totalRange)

	var expandStep int64

	switch {
	case usageRate < 0.2:
		// 起步期：固定小步长，快速响应初始需求
		expandStep = max(int64(float64(totalRange)*0.05), 5)

	case usageRate >= 0.2 && usageRate < 0.7:
		// 爆发期：按当前动态范围的比例增长
		dynamicSize := currentSize - minSize
		expandStep = max(int64(float64(dynamicSize)*0.5), 10)

	default:
		// 收敛期：剩余空间的小比例，避免过度扩容
		remaining := maxSize - currentSize
		expandStep = max(int64(float64(remaining)*0.15), 1)
	}

	// 确保不超过最大限制
	if currentSize+expandStep > maxSize {
		expandStep = maxSize - currentSize
	}

	return expandStep
}

// 非线性缩容算法
func (a *PoolManagerActor[T]) calculateShrinkSize(s *PoolManagerState[T]) int64 {
	currentSize := a.poolTotalSize.Load()
	minSize := s.config.MinSize
	idleCount := int64(len(a.sharedResources))

	if currentSize <= minSize {
		return 0
	}

	// 空闲率
	idleRate := float64(idleCount) / float64(currentSize)

	var shrinkStep int64

	switch {
	case idleRate > 0.8:
		// 大量空闲：激进缩容
		excess := currentSize - minSize
		shrinkStep = int64(float64(excess) * 0.5)

	case idleRate > 0.5:
		// 中等空闲：温和缩容
		excess := currentSize - minSize
		shrinkStep = int64(float64(excess) * 0.2)

	default:
		// 少量空闲：保守缩容
		shrinkStep = max(int64(float64(idleCount)*0.3), 1)
	}

	// 确保不低于最小值
	if currentSize-shrinkStep < minSize {
		shrinkStep = currentSize - minSize
	}

	return shrinkStep
}

func (a *PoolManagerActor[T]) checkAndAdjust(s *PoolManagerState[T]) {
	if !a.initialized.Load() {
		return
	}

	poolLen := int64(len(a.sharedResources))
	poolCap := int64(cap(a.sharedResources))
	currentSize := a.poolTotalSize.Load()

	// 扩容条件：空闲少且未达上限
	if poolLen < poolCap/5 && currentSize < s.config.MaxSize {
		a.expand(s)
	}

	// 缩容条件：空闲多且超过最小值
	if poolLen > poolCap*4/5 && currentSize > s.config.MinSize {
		a.shrink(s)
	}

	// 清理超时资源
	a.cleanupIdleTimeout(s)
}

func (a *PoolManagerActor[T]) expand(s *PoolManagerState[T]) {
	expandSize := a.calculateExpandSize(s)
	if expandSize <= 0 {
		return
	}

	// 限制单次扩容的并发创建数，避免资源爆炸
	maxConcurrent := int64(10)
	if expandSize > maxConcurrent {
		expandSize = maxConcurrent
	}

	var wg sync.WaitGroup
	for i := int64(0); i < expandSize; i++ {
		// 先预占总数
		if a.poolTotalSize.Load() >= s.config.MaxSize {
			break
		}
		a.poolTotalSize.Add(1)

		wg.Add(1)
		go func(idx int64) {
			defer wg.Done()

			conn, err := a.connControl.Create()
			if err != nil {
				a.poolTotalSize.Add(-1)
				return
			}
			res := &resource[T]{
				ID:         fmt.Sprintf("exp-%d-%d", time.Now().UnixNano(), idx),
				createTime: time.Now(),
				updateTime: time.Now(),
				Conn:       conn,
				retryCount: 0,
			}
			select {
			case a.sharedResources <- res:
			default:
				a.connControl.Close(conn)
				a.poolTotalSize.Add(-1)
			}
		}(i)
	}

	// 等待所有创建完成，避免过快触发下一轮扩容
	wg.Wait()
}

func (a *PoolManagerActor[T]) shrink(s *PoolManagerState[T]) {
	shrinkSize := a.calculateShrinkSize(s)
	if shrinkSize <= 0 {
		return
	}

	closedCount := int64(0)
	for closedCount < shrinkSize {
		select {
		case r := <-a.sharedResources:
			a.connControl.Close(r.Conn)
			a.poolTotalSize.Add(-1)
			closedCount++
		default:
			return
		}
	}
}

func (a *PoolManagerActor[T]) cleanupIdleTimeout(s *PoolManagerState[T]) {
	if s.config.SurviveTime <= 0 {
		return
	}

	expiration := time.Now().Add(-s.config.SurviveTime)
	temp := make([]*resource[T], 0, len(a.sharedResources))

	for {
		select {
		case r := <-a.sharedResources:
			if r.updateTime.Before(expiration) {
				a.connControl.Close(r.Conn)
				a.poolTotalSize.Add(-1)
			} else {
				temp = append(temp, r)
			}
		default:
			for _, r := range temp {
				select {
				case a.sharedResources <- r:
				default:
					a.connControl.Close(r.Conn)
					a.poolTotalSize.Add(-1)
				}
			}
			return
		}
	}
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
