package closure

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Actor 定义了 Actor 的基本行为接口
type Actor[T any] interface {
	// Init 初始化状态
	Init() T
	// OnStart 在 Actor 启动时调用（可选实现）
	OnStart(state *T)
	// OnStop 在 Actor 停止时调用（可选实现）
	OnStop(state *T)
}

// BaseActor 提供默认实现
type BaseActor[T any] struct{}

func (b BaseActor[T]) Init() T {
	var zero T
	return zero
}

func (b BaseActor[T]) OnStart(state *T) {}
func (b BaseActor[T]) OnStop(state *T)  {}

// Closure 是一个类型安全的 Actor 实现
type Closure[T any, A Actor[T]] struct {
	inbox   chan func()
	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once

	state T
	actor A

	// 配置选项
	inboxSize int
	timeout   time.Duration
}

// Option 配置选项
type Option func(*config)

type config struct {
	inboxSize int
	timeout   time.Duration
}

// WithInboxSize 设置消息队列大小
func WithInboxSize(size int) Option {
	return func(c *config) {
		c.inboxSize = size
	}
}
func (c *Closure[T, A]) GetActor() A {
	return c.actor
}

// WithTimeout 设置调用超时时间
func WithTimeout(timeout time.Duration) Option {
	return func(c *config) {
		c.timeout = timeout
	}
}

// New 创建一个新的 Closure Actor
func New[T any, A Actor[T]](actor A, opts ...Option) *Closure[T, A] {
	cfg := &config{
		inboxSize: 100,
		timeout:   0, // 默认不超时
	}
	for _, opt := range opts {
		opt(cfg)
	}

	c := &Closure[T, A]{
		inbox:     make(chan func(), cfg.inboxSize),
		stop:      make(chan struct{}),
		stopped:   make(chan struct{}),
		actor:     actor,
		state:     actor.Init(),
		inboxSize: cfg.inboxSize,
		timeout:   cfg.timeout,
	}

	// 启动事件循环
	go c.eventLoop()

	return c
}

// eventLoop 事件循环
func (c *Closure[T, A]) eventLoop() {
	defer close(c.stopped)

	// 调用启动钩子
	c.actor.OnStart(&c.state)

	for {
		select {
		case fn := <-c.inbox:
			fn()
		case <-c.stop:
			// 调用停止钩子
			c.actor.OnStop(&c.state)
			return
		}
	}
}

// Call 同步调用，阻塞等待结果返回
// 适用场景：需要立即获取操作结果的场景，例如查询操作或需要确认执行结果
// 注意：调用会阻塞当前 goroutine 直到操作完成
func (c *Closure[T, A]) Call(fn func(A, *T) any) (any, error) {
	return c.CallWithContext(context.Background(), fn)
}

// CallWithContext 带上下文的同步调用
// 上下文作用：
// 1. 超时控制：可以设置操作的最大执行时间，避免无限等待
// 2. 取消传播：当上层操作取消时，可以级联取消正在等待的调用
// 3. 请求追踪：可以在分布式系统中传递 trace id 等信息
// 使用场景：需要超时控制或取消机制的同步调用
// CallWithContext 带上下文的同步调用
func (c *Closure[T, A]) CallWithContext(ctx context.Context, fn func(A, *T) any) (any, error) {
	// 优先检查是否已经停止，避免向已关闭的循环发送消息导致永久阻塞
	select {
	case <-c.stopped:
		return nil, fmt.Errorf("actor is stopped")
	default:
	}

	reply := make(chan any, 1)
	errCh := make(chan error, 1)

	task := func() {
		defer func() {
			if r := recover(); r != nil {
				errCh <- fmt.Errorf("panic in call: %v", r)
			}
		}()
		reply <- fn(c.actor, &c.state)
	}

	// 尝试发送任务
	select {
	case c.inbox <- task:
		// 发送成功，继续等待结果
	case <-c.stopped:
		return nil, fmt.Errorf("actor is stopped")
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// 等待结果或超时/取消
	select {
	case result := <-reply:
		return result, nil
	case err := <-errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// CallTyped 类型安全的调用
// CallTyped 类型安全的调用（替换整个函数）
func CallTyped[T any, A Actor[T], R any](c *Closure[T, A], fn func(A, *T) R) (R, error) {
	result, callErr := c.Call(func(a A, t *T) any {
		return fn(a, t)
	})
	if callErr != nil {
		var zero R
		return zero, callErr
	}

	var zero R

	// 处理 result == nil 的情况
	if result == nil {
		// 如果 R 是 error 接口类型，则 nil 表示成功的 nil error
		if any(zero) == nil { // 利用 nil 接口相等性判断 R 是否 error
			// 返回零值（对于 error 是 nil） + 无错误
			return *new(R), nil
		}
		// 其他类型返回 nil 是异常
		return zero, fmt.Errorf("unexpected nil result for type %T", zero)
	}

	// 正常断言
	val, ok := result.(R)
	if !ok {
		return zero, fmt.Errorf("type assertion failed: expected %T, got %T", zero, result)
	}

	return val, nil
}

// Send 异步发送消息，不等待结果返回
// 适用场景：
// 1. 只需要触发操作而不关心结果，例如日志记录、事件通知
// 2. 性能敏感的场景，避免阻塞调用方
// 3. 批量操作，需要快速提交多个任务
// 注意：如果队列满会阻塞，使用 TrySend 可以避免阻塞
func (c *Closure[T, A]) Send(fn func(A, *T)) error {
	task := func() {
		defer func() {
			if r := recover(); r != nil {
				// 可以添加日志记录
				fmt.Printf("panic in send: %v\n", r)
			}
		}()
		fn(c.actor, &c.state)
	}

	select {
	case c.inbox <- task:
		return nil
	case <-c.stopped:
		return fmt.Errorf("actor is stopped")
	}
}

// TrySend 尝试异步发送，如果队列满则立即返回错误
// 适用场景：高并发环境下，需要快速失败而不是阻塞等待
func (c *Closure[T, A]) TrySend(fn func(A, *T)) error {
	task := func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("panic in try send: %v\n", r)
			}
		}()
		fn(c.actor, &c.state)
	}

	select {
	case c.inbox <- task:
		return nil
	case <-c.stopped:
		return fmt.Errorf("actor is stopped")
	default:
		return fmt.Errorf("inbox is full")
	}
}

// Stop 停止 Actor
func (c *Closure[T, A]) Stop() {
	c.once.Do(func() {
		close(c.stop)
	})
}

// StopAndWait 停止 Actor 并等待完全停止
func (c *Closure[T, A]) StopAndWait() {
	c.Stop()
	<-c.stopped
}

// IsStopped 检查是否已停止
func (c *Closure[T, A]) IsStopped() bool {
	select {
	case <-c.stopped:
		return true
	default:
		return false
	}
}

// GetState 获取状态的只读副本（需要状态实现深拷贝）
func (c *Closure[T, A]) GetState(copier func(T) T) (T, error) {
	result, err := c.Call(func(a A, t *T) any {
		return copier(*t)
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return result.(T), nil
}
