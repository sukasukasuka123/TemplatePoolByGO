package pool

import (
	"context"
	"time"
)

// 该程序主要负责池子对于资源的回收
func (p *Pool[T]) resetResource(resource *resource[T], ctx context.Context) error {
	// 如果 ctx 没有 deadline，就加一个默认的
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second) // 默认5s
		defer cancel()
	}
	err := p.connControl.Reset(resource.Conn) // 重置资源
	if err != nil {
		return err
	}
	resource.updateTime = time.Now() // 更新时间
	select {
	case p.Resources <- resource:
		return nil
	case <-ctx.Done():
		p.closeResource(resource)
		return ctx.Err()
	default:
		select {
		case p.shrinkChan <- struct{}{}: // 发送信号，触发缩容
			return nil
		default: // 防止阻塞
			p.closeResource(resource)
			return nil
		}
	}
}
