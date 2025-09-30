package pool

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// NewPool 创建一个新的资源池（原子无锁版）
func NewPool[T any](
	minisize, maxsize, expandSizeAtOnce, shrinkSizeAtOnce int64,
	surviveTime time.Duration,
	connControl Conn[T],
) (*Pool[T], error) {

	if minisize <= 0 || maxsize <= 0 || minisize > maxsize {
		return nil, fmt.Errorf("invalid pool size config: minisize=%d, maxsize=%d", minisize, maxsize)
	}
	if connControl == nil {
		return nil, fmt.Errorf("connControl must not be nil")
	}

	// 创建最小容量 channel
	ch := make(chan *resource[T], minisize)
	var atomicCh atomic.Pointer[chan *resource[T]]
	atomicCh.Store(&ch)

	// 初始化 Pool
	p := &Pool[T]{
		Resources:        &atomicCh,
		minisize:         minisize,
		maxsize:          maxsize,
		nowsize:          0,
		nowPoolsize:      minisize,
		expandSizeAtOnce: expandSizeAtOnce,
		shrinkSizeAtOnce: shrinkSizeAtOnce,
		surviveTime:      surviveTime,
		connControl:      connControl,
		expandChan:       make(chan struct{}, 1),
		shrinkChan:       make(chan struct{}, 1),
		stopChan:         make(chan struct{}),
	}

	// 预创建最小数量的资源
	for i := int64(0); i < minisize; i++ {
		conn, err := connControl.Create()
		if err != nil {
			return nil, fmt.Errorf("failed to create initial resource: %w", err)
		}
		res := &resource[T]{
			ID:         uuid.New().String(),
			createTime: time.Now(),
			updateTime: time.Now(),
			Conn:       conn,
		}

		// 放入 channel
		resCh := p.Resources.Load()
		*resCh <- res
		p.nowsize++
	}

	// 启动扩容/缩容监听
	go p.listenForExpandSignal()
	go p.listenForShrinkSignal()

	// 启动监控
	p.once.Do(func() {
		go p.monitor()
	})

	return p, nil
}

// 监听扩容信号的协程
func (p *Pool[T]) listenForExpandSignal() {
	for range p.expandChan {
		p.asyncExpand()
	}
}

// 监听缩容信号的协程
func (p *Pool[T]) listenForShrinkSignal() {
	for range p.shrinkChan {
		p.asyncShrink()
	}
}

// monitor 定期检查连接池中资源的健康状况和存活时间
func (p *Pool[T]) monitor() {
	// 设置检查间隔，比如每 30 秒检查一次
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.checkAndClean()
		case <-p.stopChan:
			// 接收到停止信号，退出循环
			return
		}
	}
}
