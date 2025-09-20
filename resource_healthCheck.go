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
// 被注释掉的是一个新的想法，但没试过可不可行
// func (p *Pool[T]) checkAndClean() {
//     var oldResources chan *resource[T]

//     p.mu.Lock()
//     if len(p.Resources) == 0 {
//         // 池子已空，直接返回，无需操作
//         p.mu.Unlock()
//         return
//     }
//     // 关键步骤：在锁内，用一个新的空通道原子性地替换旧通道
//     oldResources = p.Resources
//     p.Resources = make(chan *resource[T], p.nowPoolsize)
//     p.mu.Unlock()

//     // 接下来，在不持有锁的情况下，处理旧通道里的资源
//     // 这个操作不会阻塞其他 GetResource 或 PutResource 调用
//     for {
//         select {
//         case res := <-oldResources:
//             // 检查健康状况
//             if time.Now().Sub(res.updateTime) > p.surviveTime || p.connControl.Ping(res.Conn) != nil {
//                 go p.closeResource(res) // 异步关闭不健康的连接
//             } else {
//                 // 如果健康，尝试放回新通道
//                 select {
//                 case p.Resources <- res:
//                     // 成功放回
//                 default:
//                     // 新通道已满，直接关闭，并发送缩容信号
//                     go p.closeResource(res)
//                     p.asyncShrinkSignal() // 一个发送缩容信号的非阻塞函数
//                 }
//             }
//         default:
//             // 旧通道已空，退出循环
//             return
//         }
//     }
// }

