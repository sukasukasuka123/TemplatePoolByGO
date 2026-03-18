// ========== redis_test.go ==========
package pool_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/sukasukasuka123/TemplatePoolByGO"

	"github.com/redis/go-redis/v9"
)

const (
	redisAddr    = "localhost:6379"
	dataSize     = 1024
	redisLogFile = "benchmark_redis.log"
)

type RedisConn struct {
	client *redis.Client
	ctx    context.Context
}

type RedisControl struct {
	addr string
}

func (rc *RedisControl) Create() (*RedisConn, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         rc.addr,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     1, // 每个 RedisConn 只用一个底层连接
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, err
	}

	return &RedisConn{
		client: client,
		ctx:    context.Background(),
	}, nil
}

func (rc *RedisControl) Reset(c *RedisConn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	return c.client.Ping(ctx).Err()
}

func (rc *RedisControl) Close(c *RedisConn) error {
	return c.client.Close()
}

func (rc *RedisControl) Ping(c *RedisConn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	return c.client.Ping(ctx).Err()
}

func BenchmarkRedis_Stress(b *testing.B) {
	payload := strings.Repeat("a", dataSize)

	logFile, err := os.OpenFile(redisLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		b.Fatalf("无法创建日志文件: %v", err)
	}
	defer logFile.Close()

	fmt.Fprintf(logFile, "\n=== Redis 压测 [%s] ===\n", time.Now().Format("2006-01-02 15:04:05"))

	for _, concurrency := range stressLevels {
		b.Run(fmt.Sprintf("concurrency=%d_redis_1kb", concurrency), func(b *testing.B) {
			config := PoolConfig{
				MinSize:          int64(stressMinSize),
				MaxSize:          int64(stressMaxSize),
				SurviveTime:      stressSurvive,
				MonitorInterval:  stressMonitor,
				IdleBufferFactor: 0.3, // Redis 压测需要更大缓冲
				MaxRetries:       3,
				RetryInterval:    500 * time.Millisecond,
				ReconnectOnGet:   true, // 开启自动重连
			}

			p := NewPool(config, &RedisControl{addr: redisAddr})
			defer p.Close()

			time.Sleep(1 * time.Second) // 预热

			var successOps atomic.Int64
			var failedOps atomic.Int64
			var maxInUse atomic.Int64
			var sampleCounter atomic.Int32
			const sampleEvery = 500

			b.ResetTimer()
			start := time.Now()

			b.SetParallelism(concurrency)
			b.RunParallel(func(pb *testing.PB) {
				key := fmt.Sprintf("test_key_%d", globalConnID.Add(1))
				for pb.Next() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

					res, err := p.Get(ctx)
					if err != nil {
						failedOps.Add(1)
						cancel()
						continue
					}

					// 执行 Redis SET 操作
					err = res.Conn.client.Set(ctx, key, payload, 0).Err()
					cancel()

					putErr := p.Put(res)

					if err != nil || putErr != nil {
						failedOps.Add(1)
					} else {
						successOps.Add(1)
					}

					// 采样峰值
					if sampleCounter.Add(1)%sampleEvery == 0 {
						stats, _ := p.Stats(context.Background())
						curr := stats["pool_in_use"]
						for {
							old := maxInUse.Load()
							if curr <= old || maxInUse.CompareAndSwap(old, curr) {
								break
							}
						}
					}
				}
			})

			duration := time.Since(start).Seconds()
			throughput := float64(successOps.Load()) / duration

			fmt.Fprintf(logFile, "[并发=%d] 成功=%d | 失败=%d | 吞吐=%.2f ops/s | 峰值InUse=%d | 耗时=%.2fs\n",
				concurrency, successOps.Load(), failedOps.Load(),
				throughput, maxInUse.Load(), duration)

			finalStats, _ := p.Stats(context.Background())
			fmt.Fprintf(logFile, "  => 最终: total=%d, available=%d\n",
				finalStats["total_size"], finalStats["pool_available"])
		})
	}
}
