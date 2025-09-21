package pool

//该程序主要负责资源池的异步动态扩容和缩容，该过程与getResource和putResource异步
import (
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

func (p *Pool[T]) asyncExpand() {
	// 原子操作防止多个对容器的操作同时进行
	if !atomic.CompareAndSwapInt32(&p.isChangeSize, 0, 1) {
		return // 已经有扩容在进行
	}
	defer atomic.StoreInt32(&p.isChangeSize, 0)
	
	// 在锁内，执行原子替换操作，确保短时间内完成
	p.mu.Lock()
	
	// 检查是否还有扩容空间
	if p.nowPoolsize >= p.maxsize {
		p.mu.Unlock()
		return
	}
	
	// 计算新的池子大小
	newSize := p.nowPoolsize + p.expandSizeAtOnce
	if newSize > p.maxsize {
		newSize = p.maxsize
	}
	
	// 创建新的、更大的通道
	newPool := make(chan *resource[T], newSize)
	
	// 尽可能将旧通道中的资源搬运到新通道（不阻塞）
	// 这段搬运操作非常快，因为它只处理旧通道中已有的资源
	for {
		select {
		case res := <-p.Resources:
			newPool <- res
		default:
			goto END_OF_MOVE
		}
	}

END_OF_MOVE:
	// 原子性地替换旧通道
	p.Resources = newPool
	p.nowPoolsize = newSize
	p.mu.Unlock()
	
	// 在锁外，异步补充新的资源，不影响其他协程的获取操作
	for i := int64(0); i < p.expandSizeAtOnce && p.nowsize < p.maxsize; i++ {
		connect, err := p.connControl.Create()
		if err != nil {
			return
		}
		res := &resource[T]{
			ID:         uuid.New().String(),
			createTime: time.Now(),
			updateTime: time.Now(),
			Conn:       connect,
		}

		select {
		case p.Resources <- res: // 成功放入新通道
			atomic.AddInt64(&p.nowsize, 1)
		default:
			p.connControl.Close(res.Conn)
			return // 池子已满，停止创建
		}
	}
}
func (p *Pool[T]) asyncShrink() {
	// 原子操作防止多个对容器的操作同时进行
	if !atomic.CompareAndSwapInt32(&p.isChangeSize, 0, 1) {
		return // 已经有缩容在进行
	}
	defer atomic.StoreInt32(&p.isChangeSize, 0)

	// 在锁内，执行原子替换操作，确保短时间内完成
	p.mu.Lock()
	
	// 检查是否需要缩容
	if p.nowPoolsize <= p.minisize {
		p.mu.Unlock()
		return
	}

	// 计算新的池子大小
	newSize := p.nowPoolsize - p.shrinkSizeAtOnce
	if newSize < p.minisize {
		newSize = p.minisize
	}

	// 创建新的、更小的通道
	newPool := make(chan *resource[T], newSize)
	
	// 关键步骤：原子性地替换旧通道
	oldPool := p.Resources
	p.Resources = newPool
	p.nowPoolsize = newSize
	p.mu.Unlock()

	// 在锁外，处理旧通道中的资源
	// 这个操作是完全非阻塞的，并且会同时处理资源筛选和关闭
	for {
		select {
		case res := <-oldPool:
			// 检查是否需要保留这个资源
			if int64(len(newPool)) < newSize {
				// 检查健康和存活时间
				if time.Now().Sub(res.updateTime) > p.surviveTime || p.connControl.Ping(res.Conn) != nil {
					p.connControl.Close(res.Conn)
				} else {
					// 尝试放回新通道，如果满了就放弃
					select {
					case newPool <- res:
						// 成功放入
					default:
						// 放弃并关闭，因为新通道已满
						p.connControl.Close(res.Conn)
					}
				}
			} else {
				// 新通道已经满了，直接关闭剩下的所有资源
				p.connControl.Close(res.Conn)
			}
		default:
			return // 旧通道已空，退出循环
		}
	}
}





