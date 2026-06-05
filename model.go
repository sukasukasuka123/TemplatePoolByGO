package pool

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	if bufferSize < 1 {
		bufferSize = 1 // 防止 IdleBufferFactor=0 时创建无缓冲 channel 导致死锁
	}

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
	if config.MonitorInterval > 0 {
		go p.monitorAndAdjust(p.closeCtx)
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

// monitorAndAdjust 定期触发缩容/扩容检查（将 MonitorInterval 配置落到实处）
func (p *Pool[T]) monitorAndAdjust(ctx context.Context) {
	interval := p.config.MonitorInterval
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
				a.checkAndAdjust(s)
			})
		}
	}
}

func (p *Pool[T]) doPingRound() {
	if p.waitQueue.Len() > 0 {
		return
	}

	batch := p.collectPingBatch(5)
	if len(batch) == 0 {
		return
	}
	p.processPingBatch(batch)
}

// collectPingBatch 从 resources channel 取最多 maxCount 个连接
// 中途有等待者进来则放回已取连接并返回 nil
func (p *Pool[T]) collectPingBatch(maxCount int) []*resource[T] {
	batch := make([]*resource[T], 0, maxCount)
	for i := 0; i < maxCount; i++ {
		if p.waitQueue.Len() > 0 {
			// 有等待者进来了，放回已取连接
			for _, r := range batch {
				p.tryReturnOrClose(r)
			}
			return nil
		}
		select {
		case r := <-p.resources:
			batch = append(batch, r)
		default:
			return batch // channel 已空
		}
	}
	return batch
}

// processPingBatch 对一批连接执行 Ping，健康则放回，不健康则驱逐
func (p *Pool[T]) processPingBatch(batch []*resource[T]) {
	for _, r := range batch {
		if err := p.connControl.Ping(r.Conn); err != nil {
			// 不健康：异步通知 Actor 关闭并检查是否需要补充
			_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
				a.connControl.Close(r.Conn)
				a.poolTotalSize.Add(-1)
				a.checkAndAdjust(s)
			})
			if p.config.OnUnhealthy != nil {
				p.config.OnUnhealthy(err)
			}
		} else {
			// 健康：优先给等待者，否则放回
			if p.waitQueue.TryDequeue(r) {
				continue
			}
			p.tryReturnOrClose(r)
		}
	}
}

// tryReturnOrClose 尝试将资源放回 channel，放回后二次检查是否有等待者
// channel 满时关闭连接
func (p *Pool[T]) tryReturnOrClose(r *resource[T]) {
	select {
	case p.resources <- r:
		// 放回后二次检查：放回瞬间可能有新等待者
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

func (p *Pool[T]) preInit(count int64, cc Conn[T]) {
	var failed int64
	for i := int64(0); i < count; i++ {
		conn, err := cc.Create()
		if err != nil {
			failed++
			log.Printf("[TemplatePoolByGO] preInit: failed to create connection %d/%d: %v", i+1, count, err)
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
	created := count - failed
	if failed > 0 {
		log.Printf("[TemplatePoolByGO] preInit: %d/%d connections created successfully, %d failed", created, count, failed)
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
		// 排空 Enqueue→Remove 竞态窗口内 TryDequeue 投递到 waiter.Ch 的资源
		// 如果不排空，该资源会永久丢失（goroutine 泄漏 + 连接泄漏）
		select {
		case delivered := <-waiter.Ch:
			// 尝试直接交给下一个等待者，放不回则放回 resources channel
			if !p.waitQueue.TryDequeue(delivered) {
				select {
				case p.resources <- delivered:
				default:
					p.connControl.Close(delivered.Conn)
					p.totalSize.Add(-1)
				}
			}
		default:
		}
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
	// 如果配置了 Get 时验证连接存活，则 Ping 检测
	if p.config.ReconnectOnGet {
		if err := p.connControl.Ping(r.Conn); err != nil {
			// Ping 失败，尝试重连
			for retry := 0; retry < p.config.MaxRetries; retry++ {
				newConn, createErr := p.connControl.Create()
				if createErr == nil {
					p.connControl.Close(r.Conn)
					r.Conn = newConn
					r.createTime = time.Now()
					r.retryCount++
					break
				}
				if retry < p.config.MaxRetries-1 {
					time.Sleep(p.config.RetryInterval)
				}
			}
		}
	}
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
