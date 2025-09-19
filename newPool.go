package pool

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// NewPool 创建一个新的连接池
func NewPool[T any](
	minisize, maxsize, expandSizeAtOnce, shrinkSizeAtOnce int64,
	surviveTime time.Duration,
	connControl Conn[T],
) (*Pool[T], error) {
	// 参数检查
	if minisize <= 0 || maxsize <= 0 || minisize > maxsize {
		return nil, fmt.Errorf("invalid pool size config: minisize=%d, maxsize=%d", minisize, maxsize)
	}
	if connControl == nil {
		return nil, fmt.Errorf("connControl must not be nil")
	}

	// 初始化 Pool
	p := &Pool[T]{
		Resources:        make(chan *resource[T], minisize),
		minisize:         minisize,
		maxsize:          maxsize,
		nowsize:          0,
		nowPoolsize:      minisize,
		expandSizeAtOnce: expandSizeAtOnce,
		shrinkSizeAtOnce: shrinkSizeAtOnce,
		surviveTime:      surviveTime,
		connControl:      connControl,
	}

	// 预先创建最小数量的资源
	for i := int64(0); i < minisize; i++ {
		conn, err := connControl.Create()
		if err != nil {
			return nil, fmt.Errorf("failed to create initial resource: %w", err)
		}
		res := &resource[T]{
			ID:         uuid.New().String(), // 其实我是想拿一个地址值转int64的
			createTime: time.Now(),
			updateTime: time.Now(),
			Conn:       conn,
		}
		p.Resources <- res
		p.nowsize++
	}
	p.expandChan = make(chan struct{}, 1) // 缓冲区为1，防止重复发送信号
	p.shrinkChan = make(chan struct{}, 1) // 缓冲区为1，防止重复发送信号
	go p.listenForShrinkSignal()          // 启动监听协程
	go p.listenForExpandSignal()          // 启动监听协程
	p.stopChan = make(chan struct{})
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
