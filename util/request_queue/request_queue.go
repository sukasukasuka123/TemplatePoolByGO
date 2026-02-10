package request_queue

import (
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

type LockFreeWaiter[T any] struct {
	Ch        chan T
	next      unsafe.Pointer
	cancelled atomic.Bool
}

type LockFreeQueue[T any] struct {
	head     unsafe.Pointer
	tail     unsafe.Pointer
	length   atomic.Int64
	chanPool sync.Pool
}

func NewLockFreeQueue[T any]() *LockFreeQueue[T] {
	q := &LockFreeQueue[T]{}
	q.chanPool.New = func() interface{} {
		return make(chan T, 1) // 必须带缓冲，防止 TryDequeue 阻塞
	}
	sentinel := &LockFreeWaiter[T]{Ch: nil}
	q.head = unsafe.Pointer(sentinel)
	q.tail = unsafe.Pointer(sentinel)
	return q
}

// Enqueue 入队（非阻塞）
func (q *LockFreeQueue[T]) Enqueue() *LockFreeWaiter[T] {
	w := &LockFreeWaiter[T]{
		Ch: q.chanPool.Get().(chan T),
	}
	w.cancelled.Store(false)

	for {
		tail := (*LockFreeWaiter[T])(atomic.LoadPointer(&q.tail))
		next := (*LockFreeWaiter[T])(atomic.LoadPointer(&tail.next))

		// 一致性检查
		if tail == (*LockFreeWaiter[T])(atomic.LoadPointer(&q.tail)) {
			if next == nil {
				// 尝试链接新节点
				if atomic.CompareAndSwapPointer(&tail.next, nil, unsafe.Pointer(w)) {
					// 成功链接，尝试更新 tail（允许失败）
					atomic.CompareAndSwapPointer(&q.tail, unsafe.Pointer(tail), unsafe.Pointer(w))
					q.length.Add(1)
					return w
				}
			} else {
				// tail 落后，帮助推进
				atomic.CompareAndSwapPointer(&q.tail, unsafe.Pointer(tail), unsafe.Pointer(next))
			}
		}
		// 失败时快速重试，不需要退避
	}
}

// TryDequeue 尝试将资源分发给队列中的第一个有效等待者（优化版）
func (q *LockFreeQueue[T]) TryDequeue(resource T) bool {
	maxAttempts := 100 // 最多尝试100次，避免无限循环
	attempts := 0

	for attempts < maxAttempts {
		attempts++

		head := (*LockFreeWaiter[T])(atomic.LoadPointer(&q.head))
		tail := (*LockFreeWaiter[T])(atomic.LoadPointer(&q.tail))
		next := (*LockFreeWaiter[T])(atomic.LoadPointer(&head.next))

		// 一致性检查
		if head != (*LockFreeWaiter[T])(atomic.LoadPointer(&q.head)) {
			continue
		}

		// 队列为空
		if next == nil {
			return false
		}

		// tail 落后，帮助推进
		if head == tail {
			atomic.CompareAndSwapPointer(&q.tail, unsafe.Pointer(tail), unsafe.Pointer(next))
			continue
		}

		// ============ 处理已取消的节点（延迟删除）============
		if next.cancelled.Load() {
			if atomic.CompareAndSwapPointer(&q.head, unsafe.Pointer(head), unsafe.Pointer(next)) {
				q.length.Add(-1)
				q.recycle(next.Ch)
			}
			// 跳过已取消的节点，继续寻找下一个
			continue
		}

		// ============ 尝试抢占等待者并分发资源 ============
		if atomic.CompareAndSwapPointer(&q.head, unsafe.Pointer(head), unsafe.Pointer(next)) {
			q.length.Add(-1)

			// 尝试发送资源（非阻塞）
			select {
			case next.Ch <- resource:
				// 成功分发资源
				return true
			default:
				// Channel 已满或关闭（理论上不应该发生）
				// 回收 channel 并继续寻找下一个等待者
				q.recycle(next.Ch)
				continue
			}
		}

		// CAS 失败，有其他协程在操作，快速重试
		// 使用渐进式退避策略
		if attempts%10 == 0 {
			runtime.Gosched() // 每10次尝试让出CPU
		}
	}

	// 达到最大尝试次数，返回失败
	return false
}

// recycle 回收 channel 到池中（异步回收）
func (q *LockFreeQueue[T]) recycle(ch chan T) {
	// 异步回收，避免阻塞主流程
	go func() {
		// 清空 channel
		select {
		case <-ch:
		default:
		}
		q.chanPool.Put(ch)
	}()
}

// Remove 标记节点为已取消（延迟删除）
func (q *LockFreeQueue[T]) Remove(w *LockFreeWaiter[T]) {
	if w != nil && w.cancelled.CompareAndSwap(false, true) {
		// 只标记为取消，物理删除由 TryDequeue 延迟完成
		// 这样可以避免复杂的并发删除逻辑
	}
}

// Len 返回队列长度（近似值）
func (q *LockFreeQueue[T]) Len() int {
	return int(q.length.Load())
}

// Clear 清空队列
func (q *LockFreeQueue[T]) Clear() {
	for {
		head := (*LockFreeWaiter[T])(atomic.LoadPointer(&q.head))
		next := (*LockFreeWaiter[T])(atomic.LoadPointer(&head.next))

		if next == nil {
			return // 队列已空
		}

		if atomic.CompareAndSwapPointer(&q.head, unsafe.Pointer(head), unsafe.Pointer(next)) {
			q.length.Add(-1)
			// 关闭 channel 以通知等待者池已关闭
			if next.Ch != nil {
				close(next.Ch)
			}
		}
	}
}
