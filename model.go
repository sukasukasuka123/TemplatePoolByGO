package pool

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	closure "github.com/sukasukasuka123/TemplatePoolByGO/util/Closure"
	"github.com/sukasukasuka123/TemplatePoolByGO/util/request_queue"
)

type Pool[T any] struct {
	resources        chan *resource[T]
	inUse            atomic.Int64
	totalSize        atomic.Int64
	manager          *closure.Closure[PoolManagerState[T], *PoolManagerActor[T]]
	waitQueue        *request_queue.LockFreeQueue[*resource[T]]
	closeCtx         context.Context
	cancel           context.CancelFunc
	config           PoolConfig
	lastExpandNotify atomic.Int64
	expanding        atomic.Int64
	connControl      Conn[T]
}

// 定义连接池相关的导出错误
var (
	ErrPoolBusy = errors.New("connection pool is busy: maximum capacity reached and wait queue is full")
)

func NewPool[T any](config PoolConfig, connControl Conn[T]) *Pool[T] {
	ctx, cancel := context.WithCancel(context.Background())

	// ===== 修复点 1：buffer 大小的正确计算 =====
	// IdleBufferFactor 的真实含义：允许多少比例的连接处于空闲状态
	// 例如：MaxSize=500, IdleBufferFactor=0.4
	//   → buffer 只需容纳 200 个空闲连接
	//   → 但 totalSize 可以达到 500（另外 300 个在使用中）
	//
	// 关键：buffer 不应该限制 totalSize，只是控制内存占用！
	bufferSize := int(float64(config.MaxSize) * config.IdleBufferFactor)

	p := &Pool[T]{
		resources:        make(chan *resource[T], bufferSize),
		waitQueue:        request_queue.NewLockFreeQueue[*resource[T]](),
		cancel:           cancel,
		config:           config,
		lastExpandNotify: atomic.Int64{},
		expanding:        atomic.Int64{},
		closeCtx:         ctx,
		connControl:      connControl,
	}

	actor := NewPoolManagerActor(config, connControl, &p.totalSize, p.waitQueue, &p.expanding)
	p.manager = closure.New(actor, closure.WithInboxSize(1000))
	actor.sharedResources = p.resources
	actor.manager = p.manager

	go p.preInit(config.MinSize, connControl)
	if config.PingInterval > 0 {
		go p.pingIdleResources(p.closeCtx)
	}
	return p
}

func (p *Pool[T]) pingIdleResources(ctx context.Context) {
	interval := p.config.PingInterval
	if p.config.PingInterval <= 0 {
		return // 明确禁用
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.doPingRound()
		}
	}
}

func (p *Pool[T]) doPingRound() {
	if p.waitQueue.Len() > 0 {
		return
	}

	batch := make([]*resource[T], 0, 5)
	for i := 0; i < 5; i++ {
		if p.waitQueue.Len() > 0 {
			// 有新的等待者进来了，把已取出的放回去
			for _, r := range batch {
				select {
				case p.resources <- r:
				default:
					p.connControl.Close(r.Conn)
					p.totalSize.Add(-1)
				}
			}
			return
		}
		select {
		case r := <-p.resources:
			batch = append(batch, r)
		default:
			// channel 空了
			goto ping
		}
	}

ping:
	for _, r := range batch {
		if err := p.connControl.Ping(r.Conn); err != nil {
			_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
				a.connControl.Close(r.Conn)
				a.poolTotalSize.Add(-1)
				a.checkAndAdjust(s)
			})
			if p.config.OnUnhealthy != nil {
				p.config.OnUnhealthy(err)
			}
		} else {
			if p.waitQueue.TryDequeue(r) {
				continue
			}
			select {
			case p.resources <- r:
				// 放回后再检查一次，处理放回瞬间进来的新等待者
				if p.waitQueue.Len() > 0 {
					select {
					case r2 := <-p.resources:
						if !p.waitQueue.TryDequeue(r2) {
							select {
							case p.resources <- r2:
							default:
								p.connControl.Close(r2.Conn)
								p.totalSize.Add(-1)
							}
						}
					default:
					}
				}
			default:
				p.connControl.Close(r.Conn)
				p.totalSize.Add(-1)
			}
		}
	}
}

func (p *Pool[T]) preInit(count int64, cc Conn[T]) {
	for i := int64(0); i < count; i++ {
		conn, err := cc.Create()
		if err != nil {
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
	_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
		a.initialized.Store(true)
	})
}

func (p *Pool[T]) Get(ctx context.Context) (*resource[T], error) {
	select {
	case r := <-p.resources:
		return p.validateAndReturn(r)
	default:
	}
	// 前置拒绝，入队前判断
	if int64(p.waitQueue.Len()) >= p.config.MaxWaitQueue {
		return nil, ErrPoolBusy
	}
	waiter := p.waitQueue.Enqueue()

	select {
	case r := <-p.resources:
		p.waitQueue.Remove(waiter)
		return p.validateAndReturn(r)
	default:
	}

	// 限流的扩容通知
	now := time.Now().UnixMilli()
	lastNotify := p.lastExpandNotify.Load()
	if now-lastNotify > 10 {
		if p.lastExpandNotify.CompareAndSwap(lastNotify, now) {
			_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
				a.checkAndAdjust(s)
			})
		}
	}

	select {
	case <-ctx.Done():
		p.waitQueue.Remove(waiter)
		return nil, ctx.Err() // 删掉原来的 ErrPoolBusy 判断
	case r, ok := <-waiter.Ch:
		if !ok {
			return nil, fmt.Errorf("pool closed")
		}
		return p.validateAndReturn(r)
	}
}

func (p *Pool[T]) Put(res *resource[T]) error {
	if res == nil {
		return nil
	}
	if err := p.connControl.Reset(res.Conn); err != nil {
		_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
			a.connControl.Close(res.Conn)
			a.poolTotalSize.Add(-1)
		})
		p.inUse.Add(-1)
		return err
	}

	if p.waitQueue.TryDequeue(res) {
		p.inUse.Add(-1)
		return nil
	}

	select {
	case p.resources <- res:
		p.inUse.Add(-1)
		return nil
	default:
	}

	p.inUse.Add(-1)
	_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
		a.connControl.Close(res.Conn)
		a.poolTotalSize.Add(-1)
	})
	return nil
}

func (p *Pool[T]) validateAndReturn(r *resource[T]) (*resource[T], error) {
	p.inUse.Add(1)
	r.updateTime = time.Now()
	return r, nil
}

func (p *Pool[T]) Close() {
	p.cancel()
	p.waitQueue.Clear()
	p.manager.StopAndWait()
	for {
		select {
		case r := <-p.resources:
			p.connControl.Close(r.Conn)
		default:
			return
		}
	}
}

func (p *Pool[T]) Stats(ctx context.Context) (map[string]int64, error) {
	return map[string]int64{
		"total_size":     p.totalSize.Load(),
		"pool_available": int64(len(p.resources)),
		"pool_in_use":    p.inUse.Load(),
		"waiting_count":  int64(p.waitQueue.Len()),
		"expanding":      p.expanding.Load(),
		"buffer_cap":     int64(cap(p.resources)), // 新增：显示 buffer 容量
	}, nil
}
