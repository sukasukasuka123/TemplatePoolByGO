// ========== pool.go ==========
package pool

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	closure "github.com/sukasukasuka123/TemplatePoolByGO/util/Closure"
)

type Pool[T any] struct {
	resources chan *resource[T]
	inUse     atomic.Int64
	totalSize atomic.Int64

	manager *closure.Closure[PoolManagerState[T], *PoolManagerActor[T]]
	cancel  context.CancelFunc
	config  PoolConfig
}

func startPoolMonitor[T any](
	ctx context.Context,
	manager *closure.Closure[PoolManagerState[T], *PoolManagerActor[T]],
	interval time.Duration,
) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
					a.checkAndAdjust(s)
				})
			}
		}
	}()
}

func (p *Pool[T]) preInitResources(count int64, cc Conn[T]) {
	for i := int64(0); i < count; i++ {
		conn, err := cc.Create()
		if err != nil {
			continue
		}
		r := &resource[T]{
			ID:         fmt.Sprintf("init-%d", i),
			createTime: time.Now(),
			updateTime: time.Now(),
			Conn:       conn,
			retryCount: 0,
		}
		p.totalSize.Add(1)
		select {
		case p.resources <- r:
		default:
			cc.Close(conn)
			p.totalSize.Add(-1)
		}
	}
	p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
		a.initialized.Store(true)
	})
}

func NewPool[T any](config PoolConfig, connControl Conn[T]) *Pool[T] {
	if config.IdleBufferFactor <= 0 {
		config.IdleBufferFactor = 1.0
	}

	ctx, cancel := context.WithCancel(context.Background())

	bufferSize := int(float64(config.MaxSize) * config.IdleBufferFactor)
	if bufferSize < int(config.MinSize) {
		bufferSize = int(config.MinSize)
	}
	sharedChan := make(chan *resource[T], bufferSize)

	p := &Pool[T]{
		resources: sharedChan,
		cancel:    cancel,
		config:    config,
	}

	actor := NewPoolManagerActor(config, connControl, &p.totalSize)
	manager := closure.New(actor,
		closure.WithInboxSize(1000),
		closure.WithTimeout(5*time.Second),
	)

	actor.sharedResources = sharedChan
	actor.manager = manager

	p.manager = manager

	p.preInitResources(config.MinSize, connControl)

	time.Sleep(50 * time.Millisecond)
	startPoolMonitor(ctx, manager, config.MonitorInterval)

	return p
}

// 重连资源
func (p *Pool[T]) reconnectResource(r *resource[T]) (*resource[T], error) {
	cc := p.manager.GetActor().connControl

	for attempt := 0; attempt <= p.config.MaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(p.config.RetryInterval)
		}

		// 先关闭旧连接
		cc.Close(r.Conn)

		// 创建新连接
		newConn, err := cc.Create()
		if err != nil {
			if attempt == p.config.MaxRetries {
				return nil, fmt.Errorf("reconnect failed after %d attempts: %w", p.config.MaxRetries, err)
			}
			continue
		}

		// 重连成功
		r.Conn = newConn
		r.updateTime = time.Now()
		r.retryCount = attempt
		return r, nil
	}

	return nil, fmt.Errorf("reconnect failed")
}

func (p *Pool[T]) Get(ctx context.Context) (*resource[T], error) {
	// 快速路径：从 channel 获取
	select {
	case r := <-p.resources:
		// 可选：验证资源有效性
		if p.config.ReconnectOnGet {
			if err := p.manager.GetActor().connControl.Ping(r.Conn); err != nil {
				// 尝试重连
				newR, reconnErr := p.reconnectResource(r)
				if reconnErr != nil {
					// 重连失败，减少总数
					p.totalSize.Add(-1)
					return nil, fmt.Errorf("resource invalid and reconnect failed: %w", reconnErr)
				}
				r = newR
			}
		}

		p.inUse.Add(1)
		r.updateTime = time.Now()
		return r, nil
	default:
	}

	// 慢路径：轮询等待或创建新资源
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-p.resources:
			// 在等待期间获取到了资源
			if p.config.ReconnectOnGet {
				if err := p.manager.GetActor().connControl.Ping(r.Conn); err != nil {
					newR, reconnErr := p.reconnectResource(r)
					if reconnErr != nil {
						p.totalSize.Add(-1)
						continue // 重连失败，继续等待
					}
					r = newR
				}
			}
			p.inUse.Add(1)
			r.updateTime = time.Now()
			return r, nil

		case <-ticker.C:
			// 尝试创建新资源
			current := p.totalSize.Load()
			if current >= p.config.MaxSize {
				continue // 已达上限，继续等待
			}

			// CAS 方式抢占创建权
			if p.totalSize.CompareAndSwap(current, current+1) {
				conn, err := p.manager.GetActor().connControl.Create()
				if err != nil {
					p.totalSize.Add(-1)
					continue // 创建失败，继续等待
				}

				r := &resource[T]{
					ID:         fmt.Sprintf("r-%d", time.Now().UnixNano()),
					createTime: time.Now(),
					updateTime: time.Now(),
					Conn:       conn,
					retryCount: 0,
				}
				p.inUse.Add(1)
				return r, nil
			}
		}
	}
}
func (p *Pool[T]) Put(res *resource[T]) error {
	if res == nil {
		return nil
	}

	err := p.manager.GetActor().connControl.Reset(res.Conn)
	if err != nil {
		// Reset 失败，尝试重连
		newR, reconnErr := p.reconnectResource(res)
		if reconnErr != nil {
			// 重连失败，关闭并减少计数
			p.totalSize.Add(-1)
			return fmt.Errorf("reset failed and reconnect failed: %w", reconnErr)
		}
		res = newR
	}

	res.updateTime = time.Now()

	select {
	case p.resources <- res:
		p.inUse.Add(-1)
		return nil
	default:
		p.inUse.Add(-1)
		return p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
			select {
			case a.sharedResources <- res:
			default:
				a.connControl.Close(res.Conn)
				p.totalSize.Add(-1)
			}
		})
	}
}

func (p *Pool[T]) Close() {
	p.cancel()
	p.manager.StopAndWait()
	for {
		select {
		case r := <-p.resources:
			p.manager.GetActor().connControl.Close(r.Conn)
		default:
			return
		}
	}
}

func (p *Pool[T]) Stats(ctx context.Context) (map[string]int64, error) {
	return map[string]int64{
		"total_size":     p.totalSize.Load(),
		"pool_capacity":  int64(cap(p.resources)),
		"pool_available": int64(len(p.resources)),
		"pool_in_use":    p.inUse.Load(),
	}, nil
}

func (p *Pool[T]) TriggerExpand() error {
	return p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
		a.expand(s)
	})
}

func (p *Pool[T]) TriggerShrink() error {
	return p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
		a.shrink(s)
	})
}
