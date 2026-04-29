package pool

import "time"

// Resource 资源包装器（导出供测试使用）
type Resource[T any] struct {
	ID         string
	createTime time.Time
	updateTime time.Time
	Conn       T
	retryCount int // 重连次数
}

// 内部使用 resource 作为别名
type resource[T any] = Resource[T]

type PoolConfig struct {
	MinSize          int64
	MaxSize          int64
	SurviveTime      time.Duration
	MonitorInterval  time.Duration
	IdleBufferFactor float64 // channel 缓冲系数

	// 等待队列配置
	MaxWaitQueue int64 // 新增，建议默认值 10000

	// 重连配置
	MaxRetries     int           // 最大重试次数
	RetryInterval  time.Duration // 重试间隔
	ReconnectOnGet bool          // Get 时是否自动重连失效资源

	// 心跳配置
	PingInterval time.Duration   // 定期 Ping 连接的间隔
	OnUnhealthy  func(err error) // 回调钩子
}

func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MinSize:          5,
		MaxSize:          100,
		SurviveTime:      30 * time.Minute,
		MonitorInterval:  10 * time.Second,
		IdleBufferFactor: 1.0,
		MaxRetries:       3,
		RetryInterval:    1 * time.Second,
		ReconnectOnGet:   true,
		PingInterval:     30 * time.Second,
		OnUnhealthy:      nil,
		MaxWaitQueue:     10000,
	}
}

type Conn[T any] interface {
	Reset(T) error
	Close(T) error
	Create() (T, error)
	Ping(T) error
}
