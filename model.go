package pool

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	closure "github.com/sukasukasuka123/TemplatePoolByGO/util/Closure"
	"github.com/sukasukasuka123/TemplatePoolByGO/util/request_queue"
)

type Pool[T any] struct {
	resources chan *resource[T]
	inUse     atomic.Int64
	totalSize atomic.Int64
	manager   *closure.Closure[PoolManagerState[T], *PoolManagerActor[T]]
	waitQueue *request_queue.LockFreeQueue[*resource[T]]
	cancel    context.CancelFunc
	config    PoolConfig
	// 扩容限频控制
	lastExpandNotify atomic.Int64
	// 新增：记录扩容中的连接数，避免重复扩容
	expanding atomic.Int64
}

func NewPool[T any](config PoolConfig, connControl Conn[T]) *Pool[T] {
	_, cancel := context.WithCancel(context.Background())
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

	// 预初始化
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
	// ============ 快速路径：直接从 channel 获取 ============
	select {
	case r := <-p.resources:
		return p.validateAndReturn(r)
	default:
	}

	// ============ 慢路径：需要等待资源 ============

	// 1. 入队等待（非阻塞）
	waiter := p.waitQueue.Enqueue()

	// 2. 入队后立即再试一次 channel（防止 race condition）
	select {
	case r := <-p.resources:
		p.waitQueue.Remove(waiter)
		return p.validateAndReturn(r)
	case <-ctx.Done():
		p.waitQueue.Remove(waiter)
		return nil, ctx.Err()
	default:
		// 确实没有可用资源，继续后续流程
	}

	// 3. 触发异步扩容检测（高敏感度：每10ms最多一次）
	now := time.Now().UnixMilli()
	lastNotify := p.lastExpandNotify.Load()
	if now-lastNotify > 10 { // 10ms 限频，压测时更敏感
		if p.lastExpandNotify.CompareAndSwap(lastNotify, now) {
			// 异步通知扩容，不阻塞 Get 流程
			_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
				a.checkAndAdjust(s)
			})
		}
	}

	// 4. 阻塞等待资源（由扩容协程或Put归还时唤醒）
	select {
	case <-ctx.Done():
		p.waitQueue.Remove(waiter)
		return nil, ctx.Err()
	case r, ok := <-waiter.Ch:
		if !ok {
			return nil, fmt.Errorf("pool closed")
		}
		return p.validateAndReturn(r)
	}
}

// Put 实现逻辑：资源直达 + 高效回收
func (p *Pool[T]) Put(res *resource[T]) error {
	if res == nil {
		return nil
	}

	// 归还前的健康检查/重置
	cc := p.manager.GetActor().connControl
	if err := cc.Reset(res.Conn); err != nil {
		p.manager.GetActor().connControl.Close(res.Conn)
		p.totalSize.Add(-1)
		p.inUse.Add(-1)
		return err
	}

	// 1. 优先尝试直接交给队列里排队的人（资源直达，避开 Channel）
	if p.waitQueue.TryDequeue(res) {
		p.inUse.Add(-1)
		return nil
	}

	// 2. 没人要，放回共享池
	select {
	case p.resources <- res:
		p.inUse.Add(-1)
	default:
		// 3. 池子溢出（MaxSize 缩容期间或并发竞争），交给 Actor 销毁
		p.inUse.Add(-1)
		_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
			a.connControl.Close(res.Conn)
			a.poolTotalSize.Add(-1)
		})
	}
	return nil
}

func (p *Pool[T]) validateAndReturn(r *resource[T]) (*resource[T], error) {
	p.inUse.Add(1)
	r.updateTime = time.Now()
	// 此处可增加 ReconnectOnGet 逻辑
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
		"expanding":      p.expanding.Load(), // 新增：扩容中的连接数
	}, nil
}
