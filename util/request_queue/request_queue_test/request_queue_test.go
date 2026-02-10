package request_queue_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	// 确保路径正确，或使用相对路径
	. "github.com/sukasukasuka123/TemplatePoolByGO/util/request_queue"
)

// 辅助函数：由于原代码没写 IsEmpty，这里手动检查 Len
func isQueueEmpty[T any](q *LockFreeQueue[T]) bool {
	return q.Len() <= 0
}

// ============ 基础功能测试 ============

func TestLockFreeQueue_BasicEnqueueDequeue(t *testing.T) {
	q := NewLockFreeQueue[int]()

	// 1. 测试初始状态
	if !isQueueEmpty(q) {
		t.Error("New queue should be empty")
	}

	// 2. 入队
	w1 := q.Enqueue()
	w2 := q.Enqueue()
	w3 := q.Enqueue()

	if q.Len() != 3 {
		t.Errorf("Expected length 3, got %d", q.Len())
	}

	// 3. 异步出队测试
	go func() {
		// 稍微等待确保入队完成（虽然无锁队列不严格要求，但为了演示顺序）
		time.Sleep(10 * time.Millisecond)
		q.TryDequeue(100)
		q.TryDequeue(200)
		q.TryDequeue(300)
	}()

	// 4. 验证接收顺序 (FIFO)
	// 注意：TryDequeue 成功后会将数据写入 w.Ch
	select {
	case val1 := <-w1.Ch:
		if val1 != 100 {
			t.Errorf("W1 expected 100, got %d", val1)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for w1")
	}

	select {
	case val2 := <-w2.Ch:
		if val2 != 200 {
			t.Errorf("W2 expected 200, got %d", val2)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for w2")
	}

	select {
	case val3 := <-w3.Ch:
		if val3 != 300 {
			t.Errorf("W3 expected 300, got %d", val3)
		}
	case <-time.After(time.Second):
		t.Fatal("Timeout waiting for w3")
	}

	if q.Len() != 0 {
		t.Errorf("Queue should be empty, got %d", q.Len())
	}
}

// ============ 超时取消测试 ============

func TestLockFreeQueue_Remove(t *testing.T) {
	q := NewLockFreeQueue[int]()
	w := q.Enqueue()

	// 模拟取消
	q.Remove(w)

	// 重要：根据你的源代码，Remove 并不 close(Ch)，
	// 而是靠 TryDequeue 遍历到它时发现 cancelled 为 true 才会进行清理。

	// 我们再入队一个正常的 w2
	w2 := q.Enqueue()

	// 执行 TryDequeue，它应该：
	// 1. 发现 w 已取消 -> 清理并跳过
	// 2. 发现 w2 正常 -> 分发资源
	success := q.TryDequeue(999)
	if !success {
		t.Fatal("Dequeue should succeed by skipping cancelled node")
	}

	select {
	case val := <-w2.Ch:
		if val != 999 {
			t.Errorf("Expected 999, got %d", val)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("W2 failed to receive resource")
	}
}

// ============ 清空队列测试 ============

func TestLockFreeQueue_Clear(t *testing.T) {
	q := NewLockFreeQueue[int]()

	waiters := make([]*LockFreeWaiter[int], 10)
	for i := 0; i < 10; i++ {
		waiters[i] = q.Enqueue()
	}

	q.Clear() // Clear 会 close 所有 channel

	if q.Len() != 0 {
		t.Errorf("Expected length 0, got %d", q.Len())
	}

	for i, w := range waiters {
		select {
		case _, ok := <-w.Ch:
			if ok {
				t.Errorf("Waiter %d channel should be closed", i)
			}
		case <-time.After(100 * time.Millisecond):
			t.Errorf("Waiter %d channel blocked, expected closed", i)
		}
	}
}

// ============ 高并发压力测试 ============

func TestLockFreeQueue_HighContention(t *testing.T) {
	q := NewLockFreeQueue[int]()
	const numOps = 2000
	var wg sync.WaitGroup
	var receivedCount atomic.Int64

	// 消费端：每个 goroutine 负责接一个数据
	for i := 0; i < numOps; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := q.Enqueue()
			select {
			case <-w.Ch:
				receivedCount.Add(1)
			case <-time.After(2 * time.Second):
				// 压力测试下允许一定延迟，但不能死锁
			}
		}()
	}

	// 生产端：不断尝试出队（分发资源）
	go func() {
		count := 0
		for count < numOps {
			if q.TryDequeue(count) {
				count++
			}
			// 给 CPU 一点喘息，模拟真实负载
			if count%100 == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()

	wg.Wait()

	if receivedCount.Load() != int64(numOps) {
		t.Errorf("Expected %d, got %d", numOps, receivedCount.Load())
	}
}
