package pool

//该程序主要负责资源池的异步动态扩容和缩容，该过程与getResource和putResource异步
import (
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

func (p *Pool[T]) asyncExpand() {
	for {
		oldCh := p.Resources.Load()
		nowSize := int64(cap(*oldCh))
		if nowSize >= p.maxsize {
			return
		}

		newSize := nowSize + p.expandSizeAtOnce
		if newSize > p.maxsize {
			newSize = p.maxsize
		}

		newCh := make(chan *resource[T], newSize)

		// 搬运旧资源（非阻塞）
		for {
			select {
			case res := <-*oldCh:
				newCh <- res
			default:
				goto DONE
			}
		}
	DONE:
		// 尝试原子替换
		if p.Resources.CompareAndSwap(oldCh, &newCh) {
			p.nowPoolsize = newSize
			break
		}
		// 替换失败，重试
	}

	// 异步创建新的资源
	for i := int64(0); i < p.expandSizeAtOnce && atomic.LoadInt64(&p.nowsize) < p.maxsize; i++ {
		conn, err := p.connControl.Create()
		if err != nil {
			return
		}
		res := &resource[T]{ID: uuid.New().String(), createTime: time.Now(), updateTime: time.Now(), Conn: conn}

		ch := p.Resources.Load()
		select {
		case *ch <- res:
			atomic.AddInt64(&p.nowsize, 1)
		default:
			p.connControl.Close(res.Conn)
			return
		}
	}
}

func (p *Pool[T]) asyncShrink() {
	for {
		oldCh := p.Resources.Load()
		nowSize := int64(cap(*oldCh))
		if nowSize <= p.minisize {
			return
		}

		newSize := nowSize - p.shrinkSizeAtOnce
		if newSize < p.minisize {
			newSize = p.minisize
		}

		newCh := make(chan *resource[T], newSize)

		// 尝试原子替换
		if p.Resources.CompareAndSwap(oldCh, &newCh) {
			p.nowPoolsize = newSize
			break
		}
		// 替换失败，重试
	}

	// 处理旧资源
	oldCh := p.Resources.Load()
	for {
		select {
		case res := <-*oldCh:
			if int64(len(*oldCh)) < p.nowPoolsize {
				if time.Since(res.updateTime) > p.surviveTime || p.connControl.Ping(res.Conn) != nil {
					p.connControl.Close(res.Conn)
					atomic.AddInt64(&p.nowsize, -1)
				} else {
					ch := p.Resources.Load()
					select {
					case *ch <- res:
					default:
						p.connControl.Close(res.Conn)
						atomic.AddInt64(&p.nowsize, -1)
					}
				}
			} else {
				p.connControl.Close(res.Conn)
				atomic.AddInt64(&p.nowsize, -1)
			}
		default:
			return
		}
	}
}
