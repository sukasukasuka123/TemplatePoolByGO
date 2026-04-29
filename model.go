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
	go p.pingIdleResources()
	return p
}

func (p *Pool[T]) pingIdleResources() {
	// 1. 创建定时器，避免死循环占用 CPU
	// 使用 config.PingInterval，如果没有配置则默认 500ms
	interval := p.config.PingInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// 2. 每次触发时检查是否有等待者
			// 如果有等待者，跳过本次心跳，让连接优先服务于业务请求
			if p.waitQueue.Len() > 0 {
				continue
			}

			// 3. 批量取出少量空闲连接进行心跳
			// 注意：不要一次性取太多，避免长时间持有资源导致无法响应突发请求
			batch := make([]*resource[T], 0, 5)
			for i := 0; i < 5; i++ {
				// 双重检查：防止在取的过程中有新请求进来
				if p.waitQueue.Len() > 0 {
					// 把已经取出来的放回去
					for _, r := range batch {
						select {
						case p.resources <- r:
						default:
							// 如果放不回去（池子满了），则关闭该连接
							p.manager.GetActor().connControl.Close(r.Conn)
							p.totalSize.Add(-1)
						}
					}
					break
				}

				select {
				case r := <-p.resources:
					batch = append(batch, r)
				default:
					// 池子空了，退出循环
					break
				}
			}

			// 4. 执行心跳检测
			cc := p.manager.GetActor().connControl
			for _, r := range batch {
				// 【关键】这里打印日志，确保你能看到心跳在运行
				// 建议只在测试模式或开启调试日志时打印，否则生产环境日志量太大
				// fmt.Printf("[Heartbeat] Pinging resource %s\n", r.ID)

				if err := cc.Ping(r.Conn); err != nil {
					// 心跳失败，销毁连接
					_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
						a.connControl.Close(r.Conn)
						a.poolTotalSize.Add(-1)
						// 可选：心跳失败后尝试扩容补充
						a.checkAndAdjust(s)
					})
					if p.config.OnUnhealthy != nil {
						p.config.OnUnhealthy(err)
					}
				} else {
					if p.waitQueue.TryDequeue(r) {
						// 已经被等待者拿走了，无需放回
						continue
					}

					select {
					case p.resources <- r:
						// 成功放回
					default:
						// 池子满了（可能在心跳期间有其他连接放回），销毁多余连接
						cc.Close(r.Conn)
						p.totalSize.Add(-1)
					}
				}
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
	// 快速路径
	select {
	case r := <-p.resources:
		return p.validateAndReturn(r)
	default:
	}

	// 慢路径
	// 1. 先尝试入队
	waiter := p.waitQueue.Enqueue()

	// 2. 再次尝试快速获取（防止入队瞬间有连接释放）
	select {
	case r := <-p.resources:
		p.waitQueue.Remove(waiter)
		return p.validateAndReturn(r)
	default:
	}

	// 3. 触发扩容检测 (移除或放宽限流)
	// 只要有人在等待，且未达到最大扩容并发限制，就应该尝试通知扩容
	// 这里可以简化为每次进入等待都发送信号，让 Actor 内部去决定是否真的扩容
	_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
		a.checkAndAdjust(s)
	})

	// 4. 阻塞等待
	select {
	case <-ctx.Done():
		p.waitQueue.Remove(waiter)
		// 记录一下是因为什么超时，方便调试
		if p.waitQueue.Len() > 10000 {
			return nil, ErrPoolBusy
		}
		return nil, ctx.Err()
	case r, ok := <-waiter.Ch:
		if !ok {
			return nil, fmt.Errorf("pool closed")
		}
		return p.validateAndReturn(r)
	}
}

// func (p *Pool[T]) Get(ctx context.Context) (*resource[T], error) {
// 	// 快速路径
// 	select {
// 	case r := <-p.resources:
// 		return p.validateAndReturn(r)
// 	default:
// 	}

// 	// 慢路径
// 	waiter := p.waitQueue.Enqueue()

// 	select {
// 	case r := <-p.resources:
// 		p.waitQueue.Remove(waiter)
// 		return p.validateAndReturn(r)
// 	case <-ctx.Done():
// 		p.waitQueue.Remove(waiter)
// 		return nil, ctx.Err()
// 	default:
// 	}

// 	// 触发扩容检测
// 	now := time.Now().UnixMilli()
// 	lastNotify := p.lastExpandNotify.Load()
// 	if now-lastNotify > 10 {
// 		if p.lastExpandNotify.CompareAndSwap(lastNotify, now) {
// 			_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
// 				a.checkAndAdjust(s)
// 			})
// 		}
// 	}

// 	// 阻塞等待
// 	select {
// 	case <-ctx.Done():

// 		if p.waitQueue.Len() > 10000 {
// 			return nil, ErrPoolBusy // 或 context.DeadlineExceeded
// 		}
// 		p.waitQueue.Remove(waiter)
// 		return nil, ctx.Err()
// 	case r, ok := <-waiter.Ch:
// 		if !ok {
// 			return nil, fmt.Errorf("pool closed")
// 		}
// 		return p.validateAndReturn(r)
// 	}
// }

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
		if p.waitQueue.Len() > 0 {
			_ = p.manager.Send(func(a *PoolManagerActor[T], s *PoolManagerState[T]) {
				select {
				case r2 := <-a.sharedResources:
					if !a.waitQueue.TryDequeue(r2) {
						select {
						case a.sharedResources <- r2:
						default:
							a.connControl.Close(r2.Conn)
							a.poolTotalSize.Add(-1)
						}
					}
				default:
				}
			})
		}
		return nil
	default:
	}
	// 3. 真的满了，销毁
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
