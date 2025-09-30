package pool

import (
	"context"
	"sync/atomic"
	"time"
)

func (p *Pool[T]) PutResource(ctx context.Context, res *resource[T]) error {
	// 更新资源时间
	res.updateTime = time.Now()
	var resCh = p.Resources.Load()
	// 尝试非阻塞放回池子
	select {
	case *resCh <- res:
		return nil
	default:
		// 池满，尝试触发扩容
		select {
		case p.expandChan <- struct{}{}:
		default:
		}
		// 关闭资源并减计数
		return p.closeResource(res)
	}
}
func (p *Pool[T]) closeResource(resource *resource[T]) error {
	err := p.connControl.Close(resource.Conn)
	if err != nil {
		return err
	}
	atomic.AddInt64(&p.nowsize, -1)
	return nil
}
