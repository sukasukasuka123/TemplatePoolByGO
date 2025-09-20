package pool

//该程序主要负责资源池的异步动态扩容和缩容，该过程与getResource和putResource异步
import (
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

func (p *Pool[T]) asyncExpand() {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 原子操作防止多个对容器的操作同时进行
	if !atomic.CompareAndSwapInt32(&p.isChangeSize, 0, 1) {
		return // 已经有扩容在进行
	}
	defer atomic.StoreInt32(&p.isChangeSize, 0)
	// 循环条件和逻辑放在一起，避免 defer 被循环“积压”
	for p.nowPoolsize < p.maxsize && p.nowPoolsize < p.nowsize {
		// 新建扩容后的资源池
		newSize := p.nowPoolsize + p.expandSizeAtOnce
		if newSize > p.maxsize {
			newSize = p.maxsize
		}
		newPool := make(chan *resource[T], newSize)
		// 将旧资源池的资源搬运到新资源池
		for i := int64(0); i < p.nowPoolsize; i++ {
			res := <-p.Resources
			newPool <- res
		}
		// 替换旧的资源池
		p.Resources = newPool
		p.nowPoolsize = newSize
		// 异步补充新的资源
		for i := int64(0); i < p.expandSizeAtOnce && p.nowsize < p.maxsize; i++ {
			connect, err := p.connControl.Create()
			if err != nil {
				return
			}
			res := resource[T]{
				ID:         uuid.New().String(), //其实我是想拿一个地址值转int64的
				createTime: time.Now(),
				updateTime: time.Now(),
				Conn:       connect,
			}

			select {
			case p.Resources <- &res: // 成功放入
				atomic.AddInt64(&p.nowsize, 1)
			default: // 池子满了，关掉资源
				p.connControl.Close(res.Conn)
			}
		}
	}
}
func (p *Pool[T]) asyncShrink() {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 原子操作防止多个对容器的操作同时进行
	if !atomic.CompareAndSwapInt32(&p.isChangeSize, 0, 1) {
		return // 已经有缩容在进行
	}
	defer atomic.StoreInt32(&p.isChangeSize, 0)
	// 循环条件和逻辑放在一起，避免 defer 被循环“积压”
	for p.nowPoolsize > p.minisize && p.nowPoolsize > p.nowsize {
		// 新建缩容后的资源池
		newSize := p.nowPoolsize - p.shrinkSizeAtOnce
		if newSize < p.minisize {
			newSize = p.minisize
		}
		newPool := make(chan *resource[T], newSize)
		// 将旧资源池的资源搬运到新资源池
		for i := int64(0); i < newSize; i++ {
			res := <-p.Resources
			newPool <- res
		}
		// 替换旧的资源池
		p.Resources = newPool
		p.nowPoolsize = newSize
		// 异步关闭多余的资源
		for i := int64(0); i < p.nowPoolsize-p.nowsize; i++ {
			res := <-p.Resources
			p.connControl.Close(res.Conn)
		}
	}
}

