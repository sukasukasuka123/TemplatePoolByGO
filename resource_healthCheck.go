package pool

import "time"

func (p *Pool[T]) checkAndClean() {
	oldResources := p.Resources.Load()
	if oldResources == nil || len(*oldResources) == 0 {
		return
	}

	// 创建新通道
	newCh := make(chan *resource[T], p.nowPoolsize)
	oldCh := p.Resources.Swap(&newCh)

	// 处理旧通道里的资源
	for {
		select {
		case res := <-*oldCh:
			if time.Since(res.updateTime) > p.surviveTime || p.connControl.Ping(res.Conn) != nil {
				go p.closeResource(res)
			} else {
				select {
				case newCh <- res:
				default:
					go p.closeResource(res)
					p.asyncShrink()
				}
			}
		default:
			return
		}
	}
}
