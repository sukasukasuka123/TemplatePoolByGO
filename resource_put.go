package pool

import (
	"context"
	"sync/atomic"
)

func (p *Pool[T]) PutResource(ctx context.Context, resource *resource[T]) error {
	go p.resetResource(resource, ctx)
	return nil
}
func (p *Pool[T]) closeResource(resource *resource[T]) error {
	p.ConnLock.Lock()
	defer p.ConnLock.Unlock()
	err := p.connControl.Close(resource.Conn)
	if err != nil {
		return err
	}
	// p.nowsize--
	atomic.AddInt64(&p.nowsize, -1)
	return nil
}
