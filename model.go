// ========== pool.go ==========
package pool

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	closure "github.com/sukasukasuka123/TemplatePoolByGO/util/Closure"
	"github.com/sukasukasuka123/TemplatePoolByGO/util/actor_lite"
)

// Waiter 定义一个等待者，内部包含一个接收通道
type waiter[T any] chan *resource[T]

// PoolState 这里的 T 对应 ActorLite 管理的状态
type PoolState[T any] struct {
	waitingQueue []waiter[T] // 等待队列
}

type Pool[T any] struct {
	resources chan *resource[T]
	inUse     atomic.Int64
	totalSize atomic.Int64

	// 原有的管理 Actor（负责扩缩容、健康检查）
	manager *closure.Closure[PoolManagerState[T], *PoolManagerActor[T]]
	// 新增的极轻量 Actor（负责 Get/Put 叫号排队）
	lite *actor_lite.ActorLite[PoolState[T]]

	cancel context.CancelFunc
	config PoolConfig
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

	// 初始化 ActorLite，初始状态是一个空的等待队列
	liteActor := actor_lite.NewActorLite(PoolState[T]{
		waitingQueue: make([]waiter[T], 0, 1024),
	})

	p := &Pool[T]{
		resources: sharedChan,
		cancel:    cancel,
		config:    config,
		lite:      liteActor,
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

// 提取验证逻辑，减少代码冗余
func (p *Pool[T]) validateAndReturn(r *resource[T]) (*resource[T], error) {
	if p.config.ReconnectOnGet {
		if err := p.manager.GetActor().connControl.Ping(r.Conn); err != nil {
			newR, reconnErr := p.reconnectResource(r)
			if reconnErr != nil {
				p.totalSize.Add(-1)
				return nil, fmt.Errorf("reconnect failed: %w", reconnErr)
			}
			r = newR
		}
	}
	p.inUse.Add(1)
	r.updateTime = time.Now()
	return r, nil
}

func (p *Pool[T]) Get(ctx context.Context) (*resource[T], error) {
	// 1. 快速路径：非阻塞尝试从 Channel 获取
	select {
	case r := <-p.resources:
		return p.validateAndReturn(r)
	default:
	}

	// 2. 慢路径：资源枯竭，进入 ActorLite 挂号排队
	myTurn := make(waiter[T], 1)

	p.lite.Do(func(s *PoolState[T]) {
		// 这里是串行的，绝对安全
		// 双重检查：在排队的一瞬间，如果正好有人还了资源，直接拿走
		select {
		case r := <-p.resources:
			myTurn <- r
		default:
			// 真的没资源了，挂号
			s.waitingQueue = append(s.waitingQueue, myTurn)
			// 触发异步扩容通知
			_ = p.manager.Send(func(a *PoolManagerActor[T], state *PoolManagerState[T]) {
				a.expand(state)
			})
		}
	})

	// 3. 阻塞等待：不耗 CPU，直到被唤醒或超时
	select {
	case <-ctx.Done():
		// 如果超时了，我们需要从排队队列中移除（可选优化，目前先简单返回）
		return nil, ctx.Err()
	case r := <-myTurn:
		if r == nil {
			return nil, fmt.Errorf("pool closed or resource nil")
		}
		return p.validateAndReturn(r)
	}
}
func (p *Pool[T]) Put(res *resource[T]) error {
	if res == nil {
		return nil
	}

	// 资源重置逻辑保持不变
	cc := p.manager.GetActor().connControl
	if err := cc.Reset(res.Conn); err != nil {
		newR, reconnErr := p.reconnectResource(res)
		if reconnErr != nil {
			p.totalSize.Add(-1)
			return reconnErr
		}
		res = newR
	}
	res.updateTime = time.Now()

	// 核心变动：还资源时，先看有没有人在 ActorLite 里排队
	p.lite.Do(func(s *PoolState[T]) {
		if len(s.waitingQueue) > 0 {
			// 叫号：把资源直接给排在第一位的人
			nextWaiter := s.waitingQueue[0]
			s.waitingQueue = s.waitingQueue[1:]
			nextWaiter <- res
			p.inUse.Add(-1)
		} else {
			// 没人排队，尝试放回 Channel
			select {
			case p.resources <- res:
				p.inUse.Add(-1)
			default:
				// Channel 也满了，交给管理 Actor 销毁或处理
				p.inUse.Add(-1)
				_ = p.manager.Send(func(a *PoolManagerActor[T], state *PoolManagerState[T]) {
					a.connControl.Close(res.Conn)
					p.totalSize.Add(-1)
				})
			}
		}
	})
	return nil
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
