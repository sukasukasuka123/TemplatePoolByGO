package pool

// 该程序主要用于在资源池中获取资源和批获取资源,以及在资源池无空闲资源的时候创造临时资源
import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// GetResource 从资源池中获取资源
func (p *Pool[T]) GetResource(get_ctx context.Context, create_ctx context.Context) (*resource[T], error) {
	// 1. 非阻塞尝试获取
	select {
	case res := <-p.Resources:
		res.updateTime = time.Now()
		return res, nil
	default:
		// 没有可用资源，继续下一步
	}

	// 2. 发送扩容信号（非阻塞）
	// 只有当池子未满时才尝试扩容
	if p.nowsize < p.maxsize {
		select {
		case p.expandChan <- struct{}{}:
			// 成功发送信号，不阻塞，继续下一步
		default:
			// 信号通道已满，忽略
		}
	}

	// 3. 阻塞等待，同时监听资源通道和上下文超时
	select {
	case res := <-p.Resources:
		// 成功获取资源
		res.updateTime = time.Now()
		return res, nil
	case <-get_ctx.Done():
		// 等待超时，尝试创建临时资源
		res, err := p.createResource(create_ctx)
		if err != nil {
			// 创建临时资源失败，返回原始超时错误
			return nil, fmt.Errorf("等待资源超时且创建临时资源失败: %w", get_ctx.Err())
		}
		// 创建成功，返回临时资源
		return res, nil
	}
}

// 创建连接的工厂函数
func (p *Pool[T]) connCreator(ctx context.Context) (T, error) {
	// 这里需要实现创建资源的逻辑，例如连接数据库、创建对象等
	p.ConnLock.Lock()
	defer p.ConnLock.Unlock()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		_, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}

	conn, err := p.connControl.Create()
	if err != nil {
		return conn, err
	}
	return conn, nil
}

// 还需要一个createResource函数，用于创建资源
func (p *Pool[T]) createResource(ctx context.Context) (*resource[T], error) {
	p.createResourceLock.Lock()         // 加锁
	defer p.createResourceLock.Unlock() // 解锁
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		_, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}

	connect, err := p.connCreator(ctx)
	if err != nil {
		return nil, err
	}
	res := resource[T]{
		ID:         uuid.New().String(), //其实我是想拿一个地址值转int64的
		createTime: time.Now(),
		updateTime: time.Now(),
		Conn:       connect,
	}
	atomic.AddInt64(&p.nowsize, 1)
	return &res, nil
}
