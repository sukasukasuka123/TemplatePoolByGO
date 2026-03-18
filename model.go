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
	cancel           context.CancelFunc
	config           PoolConfig
	lastExpandNotify atomic.Int64
	expanding        atomic.Int64
}

// 定义连接池相关的导出错误
var (
	ErrPoolBusy = errors.New("connection pool is busy: maximum capacity reached and wait queue is full")
)

func NewPool[T any](config PoolConfig, connControl Conn[T]) *Pool[T] {
	_, cancel := context.WithCancel(context.Background())

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
	}

	actor := NewPoolManagerActor(config, connControl, &p.totalSize, p.waitQueue, &p.expanding)
	p.manager = closure.New(actor, closure.WithInboxSize(1000))
	actor.sharedResources = p.resources
	actor.manager = p.manager

	go p.preInit(config.MinSize, connControl)

	return p
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
	// 快速路径
	select {
	case r := <-p.resources:
		return p.validateAndReturn(r)
	default:
	}

	// 慢路径
	waiter := p.waitQueue.Enqueue()

	select {
	case r := <-p.resources:
		p.waitQueue.Remove(waiter)
		return p.validateAndReturn(r)
	case <-ctx.Done():
		p.waitQueue.Remove(waiter)
		return nil, ctx.Err()
	default:
	}

	// 触发扩容检测
	now := time.Now().UnixMilli()
	lastNotify := p.lastExpandNotify.Load()
	if now-lastNotify > 10 {
		if p.lastExpandNotify.CompareAndSwap(lastNotify, now) {
			_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
				a.checkAndAdjust(s)
			})
		}
	}

	// 阻塞等待
	select {
	case <-ctx.Done():

		if p.waitQueue.Len() > 10000 {
			return nil, ErrPoolBusy // 或 context.DeadlineExceeded
		}
		p.waitQueue.Remove(waiter)
		return nil, ctx.Err()
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

	cc := p.manager.GetActor().connControl
	if err := cc.Reset(res.Conn); err != nil {
		_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
			a.connControl.Close(res.Conn)
			a.poolTotalSize.Add(-1)
		})
		p.inUse.Add(-1)
		return err
	}

	// 1. 资源直达
	if p.waitQueue.TryDequeue(res) {
		p.inUse.Add(-1)
		return nil
	}

	// 2. 放回池子
	select {
	case p.resources <- res:
		p.inUse.Add(-1)
	default:
		// 3. 池子满了（这是正常的！说明空闲连接很多）
		// 但不应该销毁，而是继续尝试或临时持有
		p.inUse.Add(-1)
		// 选项 A：重试放回（推荐）
		select {
		case p.resources <- res:
			return nil
		default:
			// 选项 B：实在放不回去，才销毁
			_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
				a.connControl.Close(res.Conn)
				a.poolTotalSize.Add(-1)
			})
		}
	}
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
			p.manager.GetActor().connControl.Close(r.Conn)
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
