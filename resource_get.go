package pool

// 该程序主要用于在资源池中获取资源和批获取资源,以及在资源池无空闲资源的时候创造临时资源
import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

func (p *Pool[T]) GetResource(getCtx context.Context, createCtx context.Context) (*resource[T], error) {
	// 1 尝试非阻塞获取
	ch := p.Resources.Load()
	select {
	case res := <-*ch:
		res.updateTime = time.Now()
		return res, nil
	default:
		// 没有可用资源，继续下一步
	}

	// 2 发送扩容信号（非阻塞）
	if atomic.LoadInt64(&p.nowsize) < p.maxsize {
		select {
		case p.expandChan <- struct{}{}:
		default:
		}
	}

	// 3 阻塞等待资源或者上下文超时
	for {
		ch = p.Resources.Load() // 每次循环都重新获取最新通道
		select {
		case res := <-*ch:
			res.updateTime = time.Now()
			return res, nil
		case <-getCtx.Done():
			// 超时，尝试创建临时资源
			res, err := p.createResource(createCtx)
			if err != nil {
				return nil, fmt.Errorf("等待资源超时且创建临时资源失败: %w", getCtx.Err())
			}
			return res, nil
		case <-time.After(50 * time.Millisecond):
			// 避免死循环阻塞，每隔 50ms 再检查一次资源
			// 可根据业务调节这个时间
		}
	}
}

// 创建连接的工厂函数
func (p *Pool[T]) connCreator(ctx context.Context) (T, error) {
	// 这里需要实现创建资源的逻辑，例如连接数据库、创建对象等
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
