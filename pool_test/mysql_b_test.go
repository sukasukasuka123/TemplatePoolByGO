// ========== mysql_test.go ==========
package pool_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/sukasukasuka123/TemplatePoolByGO"

	_ "github.com/go-sql-driver/mysql"
)

const (
	mysqlDSN     = "root:Hzr040622@tcp(127.0.0.1:3306)/bench?charset=utf8mb4&parseTime=true&loc=Local"
	mysqlLogFile = "benchmark_mysql.log"
	mysqlDataKB  = 1024
)

// ================= MySQL 连接定义 =================

type MySQLConn struct {
	db *sql.DB
}

type MySQLControl struct{}

func (mc *MySQLControl) Create() (*MySQLConn, error) {
	db, err := sql.Open("mysql", mysqlDSN)
	if err != nil {
		return nil, err
	}

	// 非常关键：每个资源 = 1 个底层连接
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}

	return &MySQLConn{db: db}, nil
}

func (mc *MySQLControl) Reset(c *MySQLConn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	return c.db.PingContext(ctx)
}

func (mc *MySQLControl) Close(c *MySQLConn) error {
	return c.db.Close()
}

func (mc *MySQLControl) Ping(c *MySQLConn) error {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	return c.db.PingContext(ctx)
}

// ================= Benchmark =================

func BenchmarkMySQL_Stress(b *testing.B) {
	payload := strings.Repeat("a", mysqlDataKB)

	logFile, err := os.OpenFile(mysqlLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		b.Fatalf("无法创建 MySQL 日志文件: %v", err)
	}
	defer logFile.Close()

	fmt.Fprintf(logFile, "\n=== MySQL 压测 [%s] ===\n", time.Now().Format("2006-01-02 15:04:05"))

	for _, concurrency := range stressLevels {
		b.Run(fmt.Sprintf("concurrency=%d_mysql_3x1kb", concurrency), func(b *testing.B) {

			config := PoolConfig{
				MinSize:          int64(stressMinSize),
				MaxSize:          int64(stressMaxSize),
				SurviveTime:      stressSurvive,
				MonitorInterval:  stressMonitor,
				IdleBufferFactor: 2.0,
				MaxRetries:       5,
				RetryInterval:    500 * time.Millisecond,
				ReconnectOnGet:   true,
			}

			p := NewPool(config, &MySQLControl{})
			defer p.Close()

			time.Sleep(1 * time.Second) // 预热

			var successOps atomic.Int64
			var failedOps atomic.Int64
			var maxInUse atomic.Int64
			var sampleCounter atomic.Int32
			const sampleEvery = 500

			stmt := "INSERT INTO bench_kv (a, b, c) VALUES (?, ?, ?)"

			b.ResetTimer()
			start := time.Now()

			b.SetParallelism(concurrency)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

					res, err := p.Get(ctx)
					if err != nil {
						failedOps.Add(1)
						cancel()
						continue
					}

					_, err = res.Conn.db.ExecContext(ctx, stmt, payload, payload, payload)
					cancel()

					putErr := p.Put(res)

					if err != nil || putErr != nil {
						failedOps.Add(1)
					} else {
						successOps.Add(1)
					}

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

			fmt.Fprintf(logFile,
				"[并发=%d] 成功=%d | 失败=%d | 吞吐=%.2f ops/s | 峰值InUse=%d | 耗时=%.2fs\n",
				concurrency,
				successOps.Load(),
				failedOps.Load(),
				throughput,
				maxInUse.Load(),
				duration,
			)

			finalStats, _ := p.Stats(context.Background())
			fmt.Fprintf(logFile,
				"  => 最终: total=%d, available=%d\n",
				finalStats["total_size"],
				finalStats["pool_available"],
			)
		})
	}
}
