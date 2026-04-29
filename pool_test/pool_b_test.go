package pool_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/sukasukasuka123/TemplatePoolByGO"
)

// 压测配置
const (
	stressMinSize = 50
	stressMaxSize = 500
	stressSurvive = 180 * time.Second
	stressMonitor = 1 * time.Second
	opsPerLevel   = 500_000
	useDuration   = 5 * time.Millisecond
)

// 并发测试级别
var stressLevels = []int{
	1000,
	2000,
	4000,
	6000,
	8000,
	10000,
	20000,
	50000,
	100000,
}

// FakeConn 模拟连接
type FakeConn struct {
	id              int64
	closed          bool
	pingErr         error
	resetErr        error
	createFailCount atomic.Int32 // 模拟创建失败次数
}

var globalConnID atomic.Int64

func NewFakeConn() *FakeConn {
	return &FakeConn{id: globalConnID.Add(1)}
}

func (c *FakeConn) Reset(_ *FakeConn) error {
	if c.closed {
		return errors.New("cannot reset closed conn")
	}
	if c.resetErr != nil {
		return c.resetErr
	}
	return nil
}

func (c *FakeConn) Close(_ *FakeConn) error {
	c.closed = true
	return nil
}

func (c *FakeConn) Create() (*FakeConn, error) {
	time.Sleep(1 * time.Millisecond)
	return NewFakeConn(), nil
}

func (c *FakeConn) Ping(_ *FakeConn) error {
	if c.closed {
		return errors.New("connection closed")
	}
	return c.pingErr
}

// FakeConnControl 控制器
type FakeConnControl struct {
	createDelay time.Duration
	failRate    float64 // 模拟失败率 0.0-1.0
	callCount   atomic.Int64
}

func (fc *FakeConnControl) Reset(c *FakeConn) error { return c.Reset(c) }
func (fc *FakeConnControl) Close(c *FakeConn) error { return c.Close(c) }
func (fc *FakeConnControl) Ping(c *FakeConn) error  { return c.Ping(c) }
func (fc *FakeConnControl) Create() (*FakeConn, error) {
	if fc.createDelay > 0 {
		time.Sleep(fc.createDelay)
	}

	// 模拟偶发创建失败
	if fc.failRate > 0 {
		count := fc.callCount.Add(1)
		if float64(count%100) < fc.failRate*100 {
			return nil, errors.New("simulated create failure")
		}
	}

	return (&FakeConn{}).Create()
}

// BenchmarkStress_GetPut_RealUse 带真实使用时间的压测
func BenchmarkStress_GetPut_RealUse(b *testing.B) {
	logFile, err := os.OpenFile("benchmark_optimized.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		b.Fatalf("无法创建日志文件: %v", err)
	}
	defer logFile.Close()

	fmt.Fprintf(logFile, "\n=== 优化后压测开始 [%s] ===\n", time.Now().Format("2006-01-02 15:04:05"))

	for _, concurrency := range stressLevels {
		b.Run(fmt.Sprintf("concurrency=%d_optimized", concurrency), func(b *testing.B) {
			// ============ 优化后的池配置 ============
			config := PoolConfig{
				MinSize:          int64(stressMinSize),
				MaxSize:          int64(stressMaxSize),
				SurviveTime:      stressSurvive,
				MonitorInterval:  stressMonitor,
				IdleBufferFactor: 0.6,
				MaxRetries:       3,
				RetryInterval:    200 * time.Millisecond, // 缩短重试间隔
				ReconnectOnGet:   false,                  // 压测时关闭重连检查
				PingInterval:     1,                      // 压测时关闭自动 Ping
				OnUnhealthy: func(err error) {
					// 可选：记录不健康事件
					fmt.Fprintf(logFile, "  [Unhealthy] %v\n", err)
				},
			}

			p := NewPool(config, &FakeConnControl{createDelay: 1 * time.Millisecond})
			defer p.Close()

			// 等待预初始化完成
			time.Sleep(1 * time.Second)

			stats, _ := p.Stats(context.Background())
			fmt.Fprintf(logFile, "\n[%s] 并发=%d | 初始状态:\n",
				time.Now().Format("15:04:05"), concurrency)
			fmt.Fprintf(logFile, "  total=%d, available=%d, in_use=%d, waiting=%d, expanding=%d\n",
				stats["total_size"], stats["pool_available"], stats["pool_in_use"],
				stats["waiting_count"], stats["expanding"])

			// ============ 统计指标 ============
			var successOps atomic.Int64
			var failedOps atomic.Int64
			var timeoutOps atomic.Int64
			var maxInUse atomic.Int64
			var maxWaiting atomic.Int64
			var maxExpanding atomic.Int64
			var totalLatency atomic.Int64 // 总延迟（微秒）

			// 采样计数器
			var sampleCounter atomic.Int32
			const sampleEvery = 500 // 每500次操作采样一次

			b.ResetTimer()
			start := time.Now()

			// ============ 并发压测 ============
			b.SetParallelism(concurrency)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					opStart := time.Now()

					// 使用较短的超时时间，测试扩容效率
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					res, err := p.Get(ctx)
					cancel()

					if err != nil {
						if err == context.DeadlineExceeded {
							timeoutOps.Add(1)
						} else {
							failedOps.Add(1)
						}
						continue
					}

					// 模拟真实使用
					time.Sleep(useDuration)

					err = p.Put(res)
					if err != nil {
						failedOps.Add(1)
						continue
					}

					// 记录成功操作和延迟
					successOps.Add(1)
					latency := time.Since(opStart).Microseconds()
					totalLatency.Add(latency)

					// ============ 采样统计峰值 ============
					if sampleCounter.Add(1)%sampleEvery == 0 {
						currentStats, _ := p.Stats(context.Background())

						// 更新峰值
						updateMax := func(target *atomic.Int64, value int64) {
							for {
								old := target.Load()
								if value <= old || target.CompareAndSwap(old, value) {
									break
								}
							}
						}

						updateMax(&maxInUse, currentStats["pool_in_use"])
						updateMax(&maxWaiting, currentStats["waiting_count"])
						updateMax(&maxExpanding, currentStats["expanding"])
					}
				}
			})

			duration := time.Since(start).Seconds()
			totalOps := successOps.Load()
			throughput := float64(totalOps) / duration
			avgLatency := float64(0)
			if totalOps > 0 {
				avgLatency = float64(totalLatency.Load()) / float64(totalOps) / 1000.0 // 转换为毫秒
			}

			// ============ 输出结果 ============
			fmt.Fprintf(logFile, "  => 压测结果:\n")
			fmt.Fprintf(logFile, "     成功操作: %d\n", successOps.Load())
			fmt.Fprintf(logFile, "     失败操作: %d\n", failedOps.Load())
			fmt.Fprintf(logFile, "     超时操作: %d\n", timeoutOps.Load())
			fmt.Fprintf(logFile, "     吞吐量: %.2f ops/s\n", throughput)
			fmt.Fprintf(logFile, "     平均延迟: %.2f ms\n", avgLatency)
			fmt.Fprintf(logFile, "     总耗时: %.2f s\n", duration)

			fmt.Fprintf(logFile, "  => 峰值指标:\n")
			fmt.Fprintf(logFile, "     最大使用中: %d\n", maxInUse.Load())
			fmt.Fprintf(logFile, "     最大等待数: %d\n", maxWaiting.Load())
			fmt.Fprintf(logFile, "     最大扩容中: %d\n", maxExpanding.Load())

			finalStats, _ := p.Stats(context.Background())
			fmt.Fprintf(logFile, "  => 最终状态:\n")
			fmt.Fprintf(logFile, "     total=%d, available=%d, in_use=%d, waiting=%d, expanding=%d\n\n",
				finalStats["total_size"], finalStats["pool_available"], finalStats["pool_in_use"],
				finalStats["waiting_count"], finalStats["expanding"])
		})
	}

	fmt.Fprintf(logFile, "=== 压测结束 [%s] ===\n", time.Now().Format("2006-01-02 15:04:05"))
}

// BenchmarkPool_GetPut 纯调度性能测试
func BenchmarkPool_GetPut(b *testing.B) {
	config := PoolConfig{
		MinSize:          50,
		MaxSize:          100,
		IdleBufferFactor: 1.0,
		MaxRetries:       3,
		RetryInterval:    100 * time.Millisecond,
		ReconnectOnGet:   false, // 性能测试关闭重连
	}
	p := NewPool(config, &FakeConnControl{})
	defer p.Close()

	time.Sleep(200 * time.Millisecond) // 等待初始化
	ctx := context.Background()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := p.Get(ctx)
			if err != nil {
				b.Fatal(err)
			}
			_ = p.Put(res)
		}
	})
}

// BenchmarkStress_GetPut 无 sleep 高并发压测
func BenchmarkStress_GetPut(b *testing.B) {
	for _, concurrency := range stressLevels {
		var UnhealthyCount atomic.Int64
		b.Run(fmt.Sprintf("concurrency=%d", concurrency), func(b *testing.B) {
			config := PoolConfig{
				MinSize:          int64(stressMinSize),
				MaxSize:          int64(stressMaxSize),
				SurviveTime:      stressSurvive,
				MonitorInterval:  stressMonitor,
				IdleBufferFactor: 0.6,
				MaxRetries:       2,
				RetryInterval:    200 * time.Millisecond,
				ReconnectOnGet:   false,
				PingInterval:     100 * time.Microsecond,
				OnUnhealthy: func(err error) {
					// 1. 记录日志
					b.Logf("[CALLBACK TRIGGERED] OnUnhealthy called with error: %v at %v\n", err, time.Now().Format(time.RFC3339Nano))
					// 2. 增加计数
					UnhealthyCount.Add(1)
				},
			}

			p := NewPool(config, &FakeConnControl{})
			defer p.Close()

			time.Sleep(300 * time.Millisecond)

			stats, _ := p.Stats(context.Background())
			b.Logf("初始状态: total=%d, available=%d, in_use=%d",
				stats["total_size"], stats["pool_available"], stats["pool_in_use"])

			var successOps atomic.Int64
			var failedGetOps atomic.Int64
			var failedPutOps atomic.Int64
			var maxInUse atomic.Int64

			b.ResetTimer()
			start := time.Now()

			b.SetParallelism(concurrency)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
					res, err := p.Get(ctx)
					cancel()
					if err != nil {
						failedGetOps.Add(1)
						continue
					}

					err = p.Put(res)
					if err != nil {
						failedPutOps.Add(1)
						continue
					}

					successOps.Add(1)

					// 更新峰值
					currentStats, _ := p.Stats(context.Background())
					currentInUse := currentStats["pool_in_use"]
					for {
						old := maxInUse.Load()
						if currentInUse <= old || maxInUse.CompareAndSwap(old, currentInUse) {
							break
						}
					}
				}
			})

			duration := time.Since(start).Seconds()
			throughput := float64(successOps.Load()) / duration

			b.Logf("并发=%d | 成功=%d | 失败=%d(Get)、%d (Put)| 吞吐=%.2f ops/s | 峰值InUse=%d | 耗时=%.2fs",
				concurrency, successOps.Load(), failedGetOps.Load(), failedPutOps.Load(),
				throughput, maxInUse.Load(), duration)

			finalStats, _ := p.Stats(context.Background())
			b.Logf("结束状态: total=%d, available=%d, in_use=%d",
				finalStats["total_size"], finalStats["pool_available"], finalStats["pool_in_use"])
		})
	}
}

// TestReconnect 测试重连机制
func TestReconnect(t *testing.T) {
	config := PoolConfig{
		MinSize:         5,
		MaxSize:         10,
		MaxRetries:      3,
		RetryInterval:   100 * time.Millisecond,
		ReconnectOnGet:  true,
		MonitorInterval: 0, // 关闭自动监控
	}

	ctrl := &FakeConnControl{}
	p := NewPool(config, ctrl)
	defer p.Close()

	time.Sleep(200 * time.Millisecond)

	// 获取一个资源
	ctx := context.Background()
	res, err := p.Get(ctx)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	// 模拟连接失效
	res.Conn.pingErr = errors.New("connection lost")

	// 放回（应该触发重连）
	err = p.Put(res)
	if err != nil {
		t.Logf("Put with reconnect: %v", err)
	}

	stats, _ := p.Stats(ctx)
	t.Logf("After reconnect: total=%d, available=%d",
		stats["total_size"], stats["pool_available"])
}

// TestDynamicScaling 测试动态扩缩容
func TestDynamicScaling(t *testing.T) {
	config := PoolConfig{
		MinSize:          5,
		MaxSize:          50,
		MonitorInterval:  500 * time.Millisecond,
		IdleBufferFactor: 0.6,
		MaxRetries:       2,
		RetryInterval:    100 * time.Millisecond,
		ReconnectOnGet:   false,
	}

	p := NewPool(config, &FakeConnControl{})
	defer p.Close()

	time.Sleep(300 * time.Millisecond)
	ctx := context.Background()

	// 初始状态
	stats, _ := p.Stats(ctx)
	t.Logf("初始: total=%d, available=%d", stats["total_size"], stats["pool_available"])

	// 模拟高负载：快速获取资源
	resources := make([]*Resource[*FakeConn], 0, 30)

	for i := 0; i < 30; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		res, err := p.Get(ctx)
		cancel()
		if err != nil {
			t.Logf("Get #%d failed: %v", i, err)
			continue
		}
		resources = append(resources, res)
	}

	time.Sleep(1 * time.Second) // 等待扩容
	stats, _ = p.Stats(ctx)
	t.Logf("高负载后: total=%d, available=%d, in_use=%d",
		stats["total_size"], stats["pool_available"], stats["pool_in_use"])

	// 释放资源
	for _, res := range resources {
		_ = p.Put(res)
	}

	time.Sleep(2 * time.Second) // 等待缩容
	stats, _ = p.Stats(ctx)
	t.Logf("释放后: total=%d, available=%d, in_use=%d",
		stats["total_size"], stats["pool_available"], stats["pool_in_use"])
}

func BenchmarkPool_WithHeartbeat(b *testing.B) {
	logFile, _ := os.OpenFile("benchmark_heartbeat.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	defer logFile.Close() // 补上

	// MaxSize 夹在并发梯度中间，让不同并发级别测到不同路径
	config := PoolConfig{
		MinSize:          20,
		MaxSize:          200, // 明确写死，不用全局变量
		IdleBufferFactor: 0.6,
		SurviveTime:      30 * time.Second,
		MonitorInterval:  5 * time.Second,
		MaxRetries:       3,
		RetryInterval:    200 * time.Millisecond,
		ReconnectOnGet:   false,
		PingInterval:     5 * time.Second, // 心跳间隔要比压测时间短，才能观察到效果
		OnUnhealthy: func(err error) {
			fmt.Fprintf(logFile, "[Unhealthy] %v\n", err)
		},
	}

	const useDuration = 10 * time.Millisecond

	for _, concurrency := range []int{50, 100, 200, 500} {
		concurrency := concurrency
		b.Run(fmt.Sprintf("concurrent-%d", concurrency), func(b *testing.B) {
			p := NewPool(config, &FakeConnControl{createDelay: 1 * time.Millisecond})
			defer p.Close()
			time.Sleep(500 * time.Millisecond)

			var timeoutCount, errorCount atomic.Int64

			b.ResetTimer()

			// 启动固定数量的 worker，每个 worker 跑 b.N/concurrency 次
			// 这样才是真正的持续并发压力
			var wg sync.WaitGroup
			opsPerWorker := b.N/concurrency + 1

			for w := 0; w < concurrency; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < opsPerWorker; i++ {
						ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
						r, err := p.Get(ctx)
						cancel()
						if err != nil {
							if errors.Is(err, context.DeadlineExceeded) {
								timeoutCount.Add(1)
							} else {
								errorCount.Add(1)
							}
							continue
						}
						time.Sleep(useDuration)
						p.Put(r)
					}
				}()
			}
			wg.Wait()

			// 输出到日志，而不是丢弃
			finalStats, _ := p.Stats(context.Background())
			fmt.Fprintf(logFile, "[concurrent=%d] timeouts=%d errors=%d final_total=%d available=%d\n",
				concurrency,
				timeoutCount.Load(), errorCount.Load(),
				finalStats["total_size"], finalStats["pool_available"])
		})
	}
}

func TestPool_StressWithHeartbeat(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过压力测试")
	}

	logFile, _ := os.OpenFile("stress_heartbeat.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	defer logFile.Close()

	config := PoolConfig{
		MinSize:          20,
		MaxSize:          200,
		IdleBufferFactor: 0.6,
		SurviveTime:      30 * time.Second,
		MonitorInterval:  500 * time.Millisecond, // 压测时调短，能看到扩容行为
		MaxRetries:       3,
		RetryInterval:    200 * time.Millisecond,
		ReconnectOnGet:   false,
		MaxWaitQueue:     10000,
		PingInterval:     2 * time.Second, // 心跳间隔设短一点，方便快速看到效果
		OnUnhealthy: func(err error) {
			fmt.Fprintf(logFile, "[Unhealthy] %v\n", err)
		},
	}

	const (
		useDuration  = 10 * time.Millisecond
		testDuration = 15 * time.Second // 每个并发级别跑 15s，让扩容/心跳都能触发
	)

	for _, concurrency := range []int{50, 100, 200, 500} {
		concurrency := concurrency
		t.Run(fmt.Sprintf("concurrent-%d", concurrency), func(t *testing.T) {
			p := NewPool(config, &FakeConnControl{createDelay: 1 * time.Millisecond})
			defer p.Close()
			time.Sleep(500 * time.Millisecond) // 等预热

			var (
				successCount atomic.Int64
				timeoutCount atomic.Int64
				errorCount   atomic.Int64
				totalLatency atomic.Int64
			)

			ctx, cancel := context.WithTimeout(context.Background(), testDuration)
			defer cancel()

			var wg sync.WaitGroup
			for w := 0; w < concurrency; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						if ctx.Err() != nil {
							return
						}

						reqCtx, reqCancel := context.WithTimeout(context.Background(), 3*time.Second)
						start := time.Now()
						r, err := p.Get(reqCtx)
						defer reqCancel()

						if err != nil {
							if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
								timeoutCount.Add(1)
							} else {
								errorCount.Add(1)
							}
							continue
						}

						time.Sleep(useDuration)
						p.Put(r)

						successCount.Add(1)
						totalLatency.Add(time.Since(start).Microseconds())
					}
				}()
			}

			// 每秒采样一次，记录池的实时状态
			go func() {
				ticker := time.NewTicker(1 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						stats, _ := p.Stats(context.Background())
						fmt.Fprintf(logFile, "[concurrent=%d] total=%d available=%d in_use=%d waiting=%d\n",
							concurrency,
							stats["total_size"], stats["pool_available"],
							stats["pool_in_use"], stats["waiting_count"])
					}
				}
			}()

			wg.Wait()

			// 汇总
			total := successCount.Load()
			avgLatency := float64(0)
			if total > 0 {
				avgLatency = float64(totalLatency.Load()) / float64(total) / 1000.0
			}
			throughput := float64(total) / testDuration.Seconds()

			finalStats, _ := p.Stats(context.Background())
			fmt.Fprintf(logFile,
				"\n[concurrent=%d] SUMMARY: success=%d timeouts=%d errors=%d throughput=%.0f ops/s avg_latency=%.2f ms final_total=%d\n\n",
				concurrency, total, timeoutCount.Load(), errorCount.Load(),
				throughput, avgLatency, finalStats["total_size"])

			// 断言：不应该有超时（MaxSize=200 足够应付 50/100/200 并发）
			if concurrency <= 200 {
				realTimeouts := timeoutCount.Load() // 包含 DeadlineExceeded 和 Canceled
				if realTimeouts > 0 {
					t.Errorf("concurrent=%d 出现 %d 次超时", concurrency, realTimeouts)
				}
			}
		})
	}
}

// 需要改动ping的返回值
func TestPool_WithHeartbeatFail(t *testing.T) {
	if testing.Short() {
		t.Skip("跳过压力测试")
	}

	// 准备日志文件
	logFile, err := os.OpenFile("stress_heartbeat.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		t.Fatalf("Failed to open log file: %v", err)
	}
	defer logFile.Close()

	// 用于统计 OnUnhealthy 被调用的次数
	var unhealthyCount atomic.Int64

	config := PoolConfig{
		MinSize:          20,
		MaxSize:          200,
		IdleBufferFactor: 0.6,
		SurviveTime:      30 * time.Second,
		MonitorInterval:  500 * time.Millisecond,
		MaxRetries:       3,
		RetryInterval:    200 * time.Millisecond,
		ReconnectOnGet:   false,
		PingInterval:     100 * time.Millisecond, // 心跳间隔设短一点，方便快速看到效果
		OnUnhealthy: func(err error) {
			// 1. 记录日志
			fmt.Fprintf(logFile, "[CALLBACK TRIGGERED] OnUnhealthy called with error: %v at %v\n", err, time.Now().Format(time.RFC3339Nano))
			// 2. 增加计数
			unhealthyCount.Add(1)
		},
	}

	const (
		useDuration  = 10 * time.Millisecond
		testDuration = 15 * time.Second
	)

	// 我们只测试 concurrent=50 的情况来验证心跳回调，因为高并发下日志会非常乱
	// 如果你想保留所有并发测试，可以把下面的循环解开注释
	concurrencies := []int{50}
	// concurrencies := []int{50, 100, 200, 500}

	for _, concurrency := range concurrencies {
		t.Run(fmt.Sprintf("concurrent-%d-HeartbeatCheck", concurrency), func(t *testing.T) {

			// 创建控制器，并开启 Ping 失败模拟
			fakeCtrl := &FakeConnControl{
				createDelay: 1 * time.Millisecond,
			}

			p := NewPool(config, fakeCtrl)
			defer p.Close()

			// 等待预热
			time.Sleep(500 * time.Millisecond)

			// 启动业务压力协程 (模拟正常业务请求)
			ctx, cancel := context.WithTimeout(context.Background(), testDuration)
			defer cancel()

			var wg sync.WaitGroup
			var successCount atomic.Int64

			for w := 0; w < concurrency; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						if ctx.Err() != nil {
							return
						}
						reqCtx, reqCancel := context.WithTimeout(ctx, 3*time.Second)
						r, err := p.Get(reqCtx)
						reqCancel()

						if err != nil {
							continue
						}
						time.Sleep(useDuration)
						p.Put(r)
						successCount.Add(1)
					}
				}()
			}

			// 启动状态监控协程
			go func() {
				ticker := time.NewTicker(1 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						stats, _ := p.Stats(context.Background())
						fmt.Fprintf(logFile, "[STATS] concurrent=%d total=%d available=%d in_use=%d waiting=%d expanding=%d\n",
							concurrency,
							stats["total_size"], stats["pool_available"],
							stats["pool_in_use"], stats["waiting_count"], stats["expanding"])
					}
				}
			}()

			// 等待测试结束
			wg.Wait()

			// === 验证部分 ===

			// 1. 检查 OnUnhealthy 是否被调用
			count := unhealthyCount.Load()
			fmt.Fprintf(logFile, "\n[RESULT] OnUnhealthy callback triggered %d times.\n", count)

			if count == 0 {
				t.Errorf("Expected OnUnhealthy callback to be triggered at least once when Ping fails, but it was never called.")
			} else {
				t.Logf("Success! OnUnhealthy callback was triggered %d times.", count)
			}

			// 2. 检查日志文件中是否有具体的错误记录
			// (这一步主要靠人工查看 stress_heartbeat.log，或者你可以读取文件内容断言)
		})
	}
}
