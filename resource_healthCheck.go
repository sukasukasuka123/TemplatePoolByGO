package pool

import "time"

func (p *Pool[T]) checkAndClean() {
	p.mu.Lock()
	defer p.mu.Unlock()

	healthyResources := make(chan *resource[T], p.nowPoolsize)
	now := time.Now()

	// 循环直到通道为空
	for {
		select {
		case res := <-p.Resources:
			// 检查健康和存活时间
			if now.Sub(res.updateTime) > p.surviveTime || p.connControl.Ping(res.Conn) != nil {
				p.closeResource(res)
				continue
			}
			healthyResources <- res
		default:
			// 通道为空，退出循环
			goto endLoop
		}
	}
endLoop:
	p.Resources = healthyResources
}
