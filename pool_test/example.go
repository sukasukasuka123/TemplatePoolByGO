package pool_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	pool "github.com/sukasukasuka123/TemplatePoolByGO"
)

var poolConfig = pool.PoolConfig{
	MinSize:          5,
	MaxSize:          100,
	SurviveTime:      30 * time.Minute,
	MonitorInterval:  10 * time.Second,
	IdleBufferFactor: 0.4,
	MaxRetries:       3,
	RetryInterval:    1 * time.Second,
	ReconnectOnGet:   true,
	PingInterval:     30 * time.Second,
	OnUnhealthy: func(err error) {
		fmt.Printf("[Unhealthy] %v\n", err)
	},
	MaxWaitQueue: 10000,
}

// ================= ExampleConn 定义 ================= //

type ExampleConn struct {
	Num atomic.Int64
}

func (e *ExampleConn) reset() error {
	e.Num.Store(0)
	return nil
}

func (e *ExampleConn) ping() error {
	e.Num.Add(1)
	return nil
}

// ================= ExampleConnControl 定义（实现 Conn[T] 接口）================= //

type ExampleConnControl struct{}

func (ecc *ExampleConnControl) Create() (*ExampleConn, error) {
	return &ExampleConn{}, nil
}

func (ecc *ExampleConnControl) Close(_ *ExampleConn) error {
	return nil
}

func (ecc *ExampleConnControl) Reset(c *ExampleConn) error {
	return c.reset()
}

func (ecc *ExampleConnControl) Ping(c *ExampleConn) error {
	return c.ping()
}

// ================= 使用示例 ================= //

func ExampleGet() {
	p := pool.NewPool(poolConfig, &ExampleConnControl{})
	defer p.Close()

	conn, err := p.Get(context.Background())
	if err != nil {
		fmt.Printf("获取连接失败: %v\n", err)
		return
	}
	fmt.Printf("获取连接成功, Num=%d\n", conn.Conn.Num.Load())
}

func ExamplePut() {
	p := pool.NewPool(poolConfig, &ExampleConnControl{})
	defer p.Close()

	conn, err := p.Get(context.Background())
	if err != nil {
		fmt.Printf("获取连接失败: %v\n", err)
		return
	}
	fmt.Printf("获取连接成功, Num=%d\n", conn.Conn.Num.Load())

	if err := p.Put(conn); err != nil {
		fmt.Printf("归还连接失败: %v\n", err)
		return
	}
	fmt.Println("归还连接成功")
}

func ExampleStats() {
	p := pool.NewPool(poolConfig, &ExampleConnControl{})
	defer p.Close()

	conn, err := p.Get(context.Background())
	if err != nil {
		fmt.Printf("获取连接失败: %v\n", err)
		return
	}
	defer p.Put(conn)

	stats, err := p.Stats(context.Background())
	if err != nil {
		fmt.Printf("获取统计信息失败: %v\n", err)
		return
	}
	fmt.Printf("total=%d available=%d in_use=%d waiting=%d\n",
		stats["total_size"], stats["pool_available"],
		stats["pool_in_use"], stats["waiting_count"])
}