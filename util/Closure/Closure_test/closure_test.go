package closure_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	closure "github.com/sukasukasuka123/TemplatePoolByGO/util/Closure"
)

// ───────────────────────────────────────────────
// 测试专用 Actor 定义
// ───────────────────────────────────────────────

type CounterActor struct {
	closure.BaseActor[int]
}

func (a *CounterActor) Init() int {
	return 0
}

func (a *CounterActor) Increment(state *int, delta int) int {
	*state += delta
	return *state
}

func (a *CounterActor) Decrement(state *int, delta int) int {
	*state -= delta
	return *state
}

func (a *CounterActor) Get(state *int) int {
	return *state
}

type BankAccount struct {
	Balance int64
	Owner   string
}

type BankAccountActor struct {
	closure.BaseActor[BankAccount]
}

func (a *BankAccountActor) Init() BankAccount {
	return BankAccount{Balance: 0, Owner: ""}
}

func (a *BankAccountActor) Deposit(state *BankAccount, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("invalid amount: %d", amount)
	}
	state.Balance += amount
	return nil
}

func (a *BankAccountActor) Withdraw(state *BankAccount, amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("invalid amount: %d", amount)
	}
	if state.Balance < amount {
		return fmt.Errorf("insufficient balance")
	}
	state.Balance -= amount
	return nil
}

func (a *BankAccountActor) GetBalance(state *BankAccount) int64 {
	return state.Balance
}

func (a *BankAccountActor) SetOwner(state *BankAccount, owner string) {
	state.Owner = owner
}

// ───────────────────────────────────────────────
// 单元测试
// ───────────────────────────────────────────────

// TestCounterBasicOperations 测试计数器基本操作
func TestCounterBasicOperations(t *testing.T) {
	actor := closure.New(&CounterActor{})
	defer actor.StopAndWait()

	tests := []struct {
		name    string
		action  func() (int, error)
		want    int
		wantErr bool
	}{
		{
			name: "initial value",
			action: func() (int, error) {
				return closure.CallTyped(actor, func(a *CounterActor, s *int) int {
					return a.Get(s)
				})
			},
			want: 0,
		},
		{
			name: "increment 10",
			action: func() (int, error) {
				return closure.CallTyped(actor, func(a *CounterActor, s *int) int {
					return a.Increment(s, 10)
				})
			},
			want: 10,
		},
		{
			name: "decrement 4",
			action: func() (int, error) {
				return closure.CallTyped(actor, func(a *CounterActor, s *int) int {
					return a.Decrement(s, 4)
				})
			},
			want: 6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.action()
			if (err != nil) != tt.wantErr {
				t.Errorf("wantErr=%v, got err=%v", tt.wantErr, err)
				return
			}
			if got != tt.want {
				t.Errorf("want %d, got %d", tt.want, got)
			}
		})
	}
}

// TestBankAccountOperations 测试银行账户操作
func TestBankAccountOperations(t *testing.T) {
	actor := closure.New(&BankAccountActor{})
	defer actor.StopAndWait()

	// 测试初始存款
	_, err := closure.CallTyped(actor, func(a *BankAccountActor, s *BankAccount) error {
		return a.Deposit(s, 10000)
	})
	if err != nil {
		t.Fatalf("initial deposit failed: %v", err)
	}

	// 测试查询余额
	balance, err := closure.CallTyped(actor, func(a *BankAccountActor, s *BankAccount) int64 {
		return a.GetBalance(s)
	})
	if err != nil {
		t.Fatalf("get balance failed: %v", err)
	}
	if balance != 10000 {
		t.Errorf("want balance 10000, got %d", balance)
	}

	// 测试取款
	_, err = closure.CallTyped(actor, func(a *BankAccountActor, s *BankAccount) error {
		return a.Withdraw(s, 3000)
	})
	if err != nil {
		t.Fatalf("withdraw failed: %v", err)
	}

	// 验证余额
	balance, err = closure.CallTyped(actor, func(a *BankAccountActor, s *BankAccount) int64 {
		return a.GetBalance(s)
	})
	if err != nil {
		t.Fatalf("get balance failed: %v", err)
	}
	if balance != 7000 {
		t.Errorf("want balance 7000, got %d", balance)
	}

	// 测试余额不足（快速版）
	_, callErr := closure.CallTyped(actor, func(a *BankAccountActor, s *BankAccount) struct{} {
		_ = a.Withdraw(s, 100000) // 忽略返回值
		return struct{}{}
	})
	if callErr != nil {
		t.Fatalf("withdraw call failed: %v", callErr)
	}
	// 但这样无法检查业务 error，所以不推荐长期使用
}

// ───────────────────────────────────────────────
// 并发测试（race 检测重点）
// ───────────────────────────────────────────────

// TestConcurrentIncrements 测试并发递增
func TestConcurrentIncrements(t *testing.T) {
	actor := closure.New(&CounterActor{}, closure.WithInboxSize(2000))
	defer actor.StopAndWait()

	const goroutines = 50
	const incPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < incPerGoroutine; j++ {
				_, _ = closure.CallTyped(actor, func(a *CounterActor, s *int) int {
					return a.Increment(s, 1)
				})
			}
		}()
	}

	wg.Wait()

	final, err := closure.CallTyped(actor, func(a *CounterActor, s *int) int {
		return a.Get(s)
	})
	if err != nil {
		t.Fatal(err)
	}

	want := goroutines * incPerGoroutine
	if final != want {
		t.Errorf("want %d, got %d", want, final)
	}
}

// TestAsyncSendManyOperations 测试大量异步发送
func TestAsyncSendManyOperations(t *testing.T) {
	actor := closure.New(&CounterActor{}, closure.WithInboxSize(500))
	defer actor.StopAndWait()

	const sends = 300

	for i := 0; i < sends; i++ {
		err := actor.Send(func(a *CounterActor, s *int) {
			a.Increment(s, 1)
		})
		if err != nil {
			t.Fatalf("send failed at %d: %v", i, err)
		}
	}

	// 等待处理完成
	time.Sleep(100 * time.Millisecond)

	val, err := closure.CallTyped(actor, func(a *CounterActor, s *int) int {
		return a.Get(s)
	})
	if err != nil {
		t.Fatal(err)
	}

	if val != sends {
		t.Errorf("want %d, got %d", sends, val)
	}
}

// TestTrySendWhenFull 测试队列满时的 TrySend
func TestTrySendWhenFull(t *testing.T) {
	actor := closure.New(&CounterActor{}, closure.WithInboxSize(2))
	defer actor.StopAndWait()

	// 填满队列
	for i := 0; i < 2; i++ {
		_ = actor.TrySend(func(a *CounterActor, s *int) {
			time.Sleep(50 * time.Millisecond)
			a.Increment(s, 1)
		})
	}

	// 第3个应该失败
	err := actor.TrySend(func(a *CounterActor, s *int) {
		a.Increment(s, 1)
	})
	if err == nil || err.Error() != "inbox is full" {
		t.Errorf("expected 'inbox is full' error, got %v", err)
	}
}

// TestCallWithTimeout 测试上下文超时
func TestCallWithTimeout(t *testing.T) {
	actor := closure.New(&CounterActor{})
	defer actor.StopAndWait()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := actor.CallWithContext(ctx, func(a *CounterActor, s *int) any {
		time.Sleep(100 * time.Millisecond) // 故意超时
		return a.Increment(s, 1)
	})

	if err == nil || err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

// TestStopPreventsNewCalls 测试停止后阻止新调用
func TestStopPreventsNewCalls(t *testing.T) {
	actor := closure.New(&CounterActor{})
	actor.Stop()

	time.Sleep(10 * time.Millisecond)

	_, err := closure.CallTyped(actor, func(a *CounterActor, s *int) int {
		return a.Get(s)
	})
	if err == nil {
		t.Error("expected error after Stop(), got nil")
	}
}

// TestConcurrentBankTransfers 测试并发银行转账（重点 race 检测）
func TestConcurrentBankTransfers(t *testing.T) {
	acc := closure.New(&BankAccountActor{}, closure.WithInboxSize(5000))
	defer acc.StopAndWait()

	// 初始资金
	_, _ = closure.CallTyped(acc, func(a *BankAccountActor, s *BankAccount) error {
		return a.Deposit(s, 200000)
	})

	const workers = 20
	const opsPerWorker = 100

	var wg sync.WaitGroup
	var successCount atomic.Int64

	wg.Add(workers * 2) // 存款 + 取款两组

	// 存款组
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerWorker; j++ {
				_, err := closure.CallTyped(acc, func(a *BankAccountActor, s *BankAccount) error {
					return a.Deposit(s, 5)
				})
				if err == nil {
					successCount.Add(1)
				}
			}
		}()
	}

	// 取款组
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerWorker; j++ {
				_, err := closure.CallTyped(acc, func(a *BankAccountActor, s *BankAccount) error {
					return a.Withdraw(s, 5)
				})
				if err == nil {
					successCount.Add(1)
				}
			}
		}()
	}

	wg.Wait()

	balance, err := closure.CallTyped(acc, func(a *BankAccountActor, s *BankAccount) int64 {
		return a.GetBalance(s)
	})
	if err != nil {
		t.Fatal(err)
	}

	// 净变化应该 ≈ 0（存取相同）
	// 允许合理误差范围，因为可能有些取款因余额不足失败
	t.Logf("Final balance: %d, success ops: %d", balance, successCount.Load())
	if balance < 150000 || balance > 250000 {
		t.Errorf("final balance out of reasonable range: %d", balance)
	}
}

// ───────────────────────────────────────────────
// 基准测试
// ───────────────────────────────────────────────

// BenchmarkCallSync 基准测试同步调用
func BenchmarkCallSync(b *testing.B) {
	counter := closure.New(&CounterActor{})
	defer counter.StopAndWait()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		closure.CallTyped(counter, func(a *CounterActor, state *int) int {
			return a.Increment(state, 1)
		})
	}
}

// BenchmarkSendAsync 基准测试异步发送
func BenchmarkSendAsync(b *testing.B) {
	counter := closure.New(&CounterActor{}, closure.WithInboxSize(100000))
	defer counter.StopAndWait()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		counter.Send(func(a *CounterActor, state *int) {
			a.Increment(state, 1)
		})
	}
}

// BenchmarkConcurrentCall 基准测试并发调用
func BenchmarkConcurrentCall(b *testing.B) {
	counter := closure.New(&CounterActor{})
	defer counter.StopAndWait()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			closure.CallTyped(counter, func(a *CounterActor, state *int) int {
				return a.Increment(state, 1)
			})
		}
	})
}

// BenchmarkBankAccount 基准测试银行账户操作
func BenchmarkBankAccount(b *testing.B) {
	account := closure.New(&BankAccountActor{})
	defer account.StopAndWait()

	// 初始存款
	closure.CallTyped(account, func(a *BankAccountActor, state *BankAccount) error {
		return a.Deposit(state, 1000000)
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			closure.CallTyped(account, func(a *BankAccountActor, state *BankAccount) error {
				return a.Deposit(state, 10)
			})
		} else {
			closure.CallTyped(account, func(a *BankAccountActor, state *BankAccount) error {
				return a.Withdraw(state, 10)
			})
		}
	}
}

// ───────────────────────────────────────────────
// 集成测试
// ───────────────────────────────────────────────

// TestIntegrationComplexWorkflow 测试复杂工作流
func TestIntegrationComplexWorkflow(t *testing.T) {
	account := closure.New(&BankAccountActor{})
	defer account.StopAndWait()

	// 设置所有者
	account.Send(func(a *BankAccountActor, state *BankAccount) {
		a.SetOwner(state, "Alice")
	})

	time.Sleep(10 * time.Millisecond) // 等待异步操作完成

	// 执行多步骤操作
	steps := []struct {
		name   string
		action func() error
	}{
		{
			name: "初始存款",
			action: func() error {
				_, err := closure.CallTyped(account, func(a *BankAccountActor, state *BankAccount) error {
					return a.Deposit(state, 5000)
				})
				return err
			},
		},
		{
			name: "第一次取款",
			action: func() error {
				_, err := closure.CallTyped(account, func(a *BankAccountActor, state *BankAccount) error {
					return a.Withdraw(state, 1000)
				})
				return err
			},
		},
		{
			name: "第二次存款",
			action: func() error {
				_, err := closure.CallTyped(account, func(a *BankAccountActor, state *BankAccount) error {
					return a.Deposit(state, 3000)
				})
				return err
			},
		},
		{
			name: "第二次取款",
			action: func() error {
				_, err := closure.CallTyped(account, func(a *BankAccountActor, state *BankAccount) error {
					return a.Withdraw(state, 2000)
				})
				return err
			},
		},
	}

	for _, step := range steps {
		if err := step.action(); err != nil {
			t.Fatalf("Step '%s' failed: %v", step.name, err)
		}
	}

	// 验证最终余额
	balance, err := closure.CallTyped(account, func(a *BankAccountActor, state *BankAccount) int64 {
		return a.GetBalance(state)
	})
	if err != nil {
		t.Fatalf("GetBalance failed: %v", err)
	}

	expected := int64(5000 - 1000 + 3000 - 2000)
	if balance != expected {
		t.Errorf("Expected final balance %d, got %d", expected, balance)
	}
}

// ───────────────────────────────────────────────
// 使用示例（Example 测试）
// ───────────────────────────────────────────────

// ExampleCounterActor 演示计数器的使用
// Call: 同步调用，阻塞等待结果返回，适合需要立即获取结果的场景
// Send: 异步发送，不等待结果，适合触发式操作
func ExampleCounterActor() {
	// 创建计数器 Actor
	counter := closure.New(&CounterActor{})
	defer counter.StopAndWait()

	// 使用 Call 进行同步调用，立即获取结果
	result, _ := closure.CallTyped(counter, func(a *CounterActor, state *int) int {
		return a.Increment(state, 5)
	})
	fmt.Printf("Counter after increment: %d\n", result)

	// 使用 Send 进行异步调用，不等待结果
	// 适合不需要立即知道结果的场景
	counter.Send(func(a *CounterActor, state *int) {
		a.Increment(state, 3)
	})

	// 稍后查询结果
	time.Sleep(10 * time.Millisecond)
	finalValue, _ := closure.CallTyped(counter, func(a *CounterActor, state *int) int {
		return a.Get(state)
	})
	fmt.Printf("Final counter value: %d\n", finalValue)

	// Output:
	// Counter after increment: 5
	// Final counter value: 8
}

// ExampleBankAccountActor 演示银行账户的使用
func ExampleBankAccountActor() {
	// 创建银行账户 Actor，设置队列大小和超时
	account := closure.New(
		&BankAccountActor{},
		closure.WithInboxSize(200),
		closure.WithTimeout(time.Second),
	)
	defer account.StopAndWait()

	// 同步存款
	closure.CallTyped(account, func(a *BankAccountActor, state *BankAccount) error {
		return a.Deposit(state, 1000)
	})

	// 同步取款
	closure.CallTyped(account, func(a *BankAccountActor, state *BankAccount) error {
		return a.Withdraw(state, 300)
	})

	// 查询余额
	balance, _ := closure.CallTyped(account, func(a *BankAccountActor, state *BankAccount) int64 {
		return a.GetBalance(state)
	})
	fmt.Printf("Account balance: %d\n", balance)

	// Output:
	// Account balance: 700
}

// ExampleClosure_CallWithContext 演示带上下文的调用
// 上下文作用：
// 1. 超时控制：设置操作的最大执行时间，避免无限等待
// 2. 取消传播：当上层操作取消时，级联取消正在等待的调用
// 3. 请求追踪：在分布式系统中传递 trace id 等信息
func ExampleClosure_CallWithContext() {
	counter := closure.New(&CounterActor{})
	defer counter.StopAndWait()

	// 创建一个带超时的上下文，避免操作执行时间过长
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// 使用上下文调用，如果超时会返回错误
	_, err := counter.CallWithContext(ctx, func(a *CounterActor, state *int) any {
		return a.Increment(state, 1)
	})

	if err != nil {
		fmt.Printf("Call failed: %v\n", err)
	} else {
		fmt.Println("Call succeeded")
	}

	// Output:
	// Call succeeded
}

// 运行命令示例：
// 单元测试: go test -v
// 竞态检测: go test -race
// 基准测试: go test -bench=. -benchmem
// 特定基准: go test -bench=BenchmarkCallSync -benchtime=5s
