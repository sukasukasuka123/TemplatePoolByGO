package pool_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	. "3DsharingSquare/util/pool"
)

// 压测配置
const (
	stressMinSize = 50
	stressMaxSize = 500
	stressSurvive = 180 * time.Second
	stressMonitor = 2 * time.Second
	opsPerLevel   = 500_000
	useDuration   = 4 * time.Millisecond
)

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
	time.Sleep(10 * time.Millisecond)
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

// 并发测试级别
var stressLevels = []int{
	1000,
	2000,
	4000,
	6000,
	8000,
	100000,
}

// BenchmarkStress_GetPut_RealUse 带真实使用时间的压测
func BenchmarkStress_GetPut_RealUse(b *testing.B) {
	logFile, err := os.OpenFile("benchmark_res.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		b.Fatalf("无法创建日志文件: %v", err)
	}
	defer logFile.Close()

	fmt.Fprintf(logFile, "\n=== 压测开始 [%s] ===\n", time.Now().Format("2006-01-02 15:04:05"))

	for _, concurrency := range stressLevels {
		b.Run(fmt.Sprintf("concurrency=%d_use4ms", concurrency), func(b *testing.B) {
			config := PoolConfig{
				MinSize:          int64(stressMinSize),
				MaxSize:          int64(stressMaxSize),
				SurviveTime:      stressSurvive,
				MonitorInterval:  stressMonitor,
				IdleBufferFactor: 2.0, // 增大缓冲以减少慢路径
				MaxRetries:       3,
				RetryInterval:    500 * time.Millisecond,
				ReconnectOnGet:   true,
			}

			p := NewPool(config, &FakeConnControl{createDelay: 10 * time.Millisecond})
			defer p.Close()

			time.Sleep(500 * time.Millisecond) // 等待预初始化

			stats, _ := p.Stats(context.Background())
			fmt.Fprintf(logFile, "[%s] 并发=%d | 初始: total=%d, available=%d, in_use=%d\n",
				time.Now().Format("15:04:05"), concurrency,
				stats["total_size"], stats["pool_available"], stats["pool_in_use"])

			var successOps atomic.Int64
			var failedOps atomic.Int64
			var maxInUse atomic.Int64
			var sampleCounter atomic.Int32
			const sampleEvery = 1000

			b.ResetTimer()
			start := time.Now()

			b.SetParallelism(concurrency)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					res, err := p.Get(ctx)
					cancel()

					if err != nil {
						failedOps.Add(1)
						continue
					}

					// 模拟真实使用
					time.Sleep(useDuration)

					err = p.Put(res)
					if err != nil {
						failedOps.Add(1)
						continue
					}

					successOps.Add(1)

					// 采样统计峰值
					if sampleCounter.Add(1)%sampleEvery == 0 {
						currentStats, _ := p.Stats(context.Background())
						currentInUse := currentStats["pool_in_use"]
						for {
							old := maxInUse.Load()
							if currentInUse <= old || maxInUse.CompareAndSwap(old, currentInUse) {
								break
							}
						}
					}
				}
			})

			duration := time.Since(start).Seconds()
			throughput := float64(successOps.Load()) / duration

			fmt.Fprintf(logFile, "  => 结果: 成功=%d | 失败=%d | 吞吐=%.2f ops/s | 峰值InUse=%d | 耗时=%.2fs\n",
				successOps.Load(), failedOps.Load(), throughput, maxInUse.Load(), duration)

			finalStats, _ := p.Stats(context.Background())
			fmt.Fprintf(logFile, "  => 最终: total=%d, available=%d, in_use=%d\n\n",
				finalStats["total_size"], finalStats["pool_available"], finalStats["pool_in_use"])
		})
	}
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
		b.Run(fmt.Sprintf("concurrency=%d", concurrency), func(b *testing.B) {
			config := PoolConfig{
				MinSize:          int64(stressMinSize),
				MaxSize:          int64(stressMaxSize),
				SurviveTime:      stressSurvive,
				MonitorInterval:  stressMonitor,
				IdleBufferFactor: 1.5,
				MaxRetries:       2,
				RetryInterval:    200 * time.Millisecond,
				ReconnectOnGet:   false,
			}

			p := NewPool(config, &FakeConnControl{})
			defer p.Close()

			time.Sleep(300 * time.Millisecond)

			stats, _ := p.Stats(context.Background())
			b.Logf("初始状态: total=%d, available=%d, in_use=%d",
				stats["total_size"], stats["pool_available"], stats["pool_in_use"])

			var successOps atomic.Int64
			var failedOps atomic.Int64
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
						failedOps.Add(1)
						continue
					}

					err = p.Put(res)
					if err != nil {
						failedOps.Add(1)
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

			b.Logf("并发=%d | 成功=%d | 失败=%d | 吞吐=%.2f ops/s | 峰值InUse=%d | 耗时=%.2fs",
				concurrency, successOps.Load(), failedOps.Load(),
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
		IdleBufferFactor: 2.0,
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
		res, err := p.Get(ctx)
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
