# 泛型资源池 (github.com/sukasukasuka123/TemplatePoolByGO)

一个基于 Go 泛型的高性能连接池实现，支持动态扩缩容、自动重连和并发安全的资源管理。

## 核心特性

- 泛型设计：支持任意类型的资源管理（数据库连接、Redis 连接等）
- 动态扩缩容：基于负载自动调整池大小
- 自动重连：连接失效时自动重连，提高可用性
- Actor 模型：使用 Closure Actor 实现线程安全的状态管理
- 高性能：快速路径优化，支持高并发场景

## 架构设计

### 核心组件

1. **Pool**: 资源池主体，负责资源的获取、归还和生命周期管理
2. **PoolManagerActor**: 基于 Actor 模型的资源管理器，负责扩缩容和监控
3. **Conn Interface**: 资源控制接口，定义资源的创建、重置、关闭和健康检查

### 工作原理

资源池采用双路径设计：

- **快速路径**：直接从 channel 获取空闲资源，无锁设计
- **慢路径**：当资源不足时，通过轮询等待或动态创建新资源

扩缩容采用非线性算法，根据资源使用率自适应调整：

- **起步期** (使用率 < 20%)：小步快速扩容
- **爆发期** (20% ≤ 使用率 < 70%)：按当前规模比例扩容
- **收敛期** (使用率 ≥ 70%)：保守扩容，避免资源浪费

## 使用示例

### Redis 连接池

```go
package main

import (
    "context"
    "time"
    "github.com/redis/go-redis/v9"
    "your-module/pool"
)

type RedisConn struct {
    client *redis.Client
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
        PoolSize:     1,
    })

    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel()

    if err := client.Ping(ctx).Err(); err != nil {
        client.Close()
        return nil, err
    }

    return &RedisConn{client: client}, nil
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

func main() {
    config := pool.PoolConfig{
        MinSize:          50,
        MaxSize:          500,
        SurviveTime:      180 * time.Second,
        MonitorInterval:  2 * time.Second,
        IdleBufferFactor: 3.0,
        MaxRetries:       3,
        RetryInterval:    500 * time.Millisecond,
        ReconnectOnGet:   true,
    }

    p := pool.NewPool(config, &RedisControl{addr: "localhost:6379"})
    defer p.Close()

    // 使用连接池
    ctx := context.Background()
    res, err := p.Get(ctx)
    if err != nil {
        panic(err)
    }

    // 执行 Redis 操作
    err = res.Conn.client.Set(ctx, "key", "value", 0).Err()
    
    // 归还资源
    p.Put(res)
}
```

### MySQL 连接池

```go
package main

import (
    "context"
    "database/sql"
    "time"
    _ "github.com/go-sql-driver/mysql"
    "your-module/pool"
)

type MySQLConn struct {
    db *sql.DB
}

type MySQLControl struct{}

func (mc *MySQLControl) Create() (*MySQLConn, error) {
    db, err := sql.Open("mysql", "user:password@tcp(127.0.0.1:3306)/database")
    if err != nil {
        return nil, err
    }

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

func main() {
    config := pool.PoolConfig{
        MinSize:          50,
        MaxSize:          500,
        SurviveTime:      180 * time.Second,
        MonitorInterval:  2 * time.Second,
        IdleBufferFactor: 2.0,
        MaxRetries:       5,
        RetryInterval:    500 * time.Millisecond,
        ReconnectOnGet:   true,
    }

    p := pool.NewPool(config, &MySQLControl{})
    defer p.Close()

    ctx := context.Background()
    res, err := p.Get(ctx)
    if err != nil {
        panic(err)
    }

    // 执行 SQL 操作
    _, err = res.Conn.db.ExecContext(ctx, "INSERT INTO table VALUES (?, ?)", val1, val2)
    
    p.Put(res)
}
```

## 性能测试

### 测试环境

- MinSize: 50
- MaxSize: 500
- MonitorInterval: 2s
- 测试负载: 每次操作 4ms 使用时间

### 模拟连接池测试（FakeConn）

| 并发数 | 成功操作 | 失败操作 | 吞吐量 (ops/s) | 峰值使用 | 耗时 (s) |
|--------|----------|----------|----------------|----------|----------|
| 1,000  | 10,000   | 0        | 67,560.58      | 498      | 0.15     |
| 1,000  | 81,006   | 0        | 49,129.52      | 499      | 1.65     |
| 2,000  | 10,000   | 0        | 64,812.64      | 499      | 0.15     |
| 2,000  | 77,814   | 0        | 39,510.34      | 498      | 1.97     |
| 4,000  | 10,000   | 0        | 50,862.66      | 498      | 0.20     |
| 4,000  | 60,957   | 0        | 44,008.22      | 499      | 1.39     |
| 6,000  | 9,847    | 0        | 52,577.03      | 499      | 0.19     |
| 6,000  | 62,147   | 889      | 25,034.93      | 499      | 2.48     |
| 8,000  | 5,242    | 0        | 72,736.96      | 499      | 0.07     |
| 8,000  | 87,076   | 201      | 39,368.72      | 498      | 2.21     |
| 10,000 | 3,508    | 0        | 58,725.94      | 498      | 0.06     |
| 10,000 | 68,384   | 2,102    | 25,571.71      | 499      | 2.67     |

### Redis 压测结果

| 并发数  | 成功操作 | 失败操作 | 吞吐量 (ops/s) | 峰值使用 | 耗时 (s) |
|---------|----------|----------|----------------|----------|----------|
| 1,000   | 2,828    | 0        | 4,980.31       | 356      | 0.57     |
| 1,000   | 5,842    | 0        | 3,845.89       | 339      | 1.52     |
| 2,000   | 4,510    | 0        | 5,404.89       | 213      | 0.83     |
| 2,000   | 6,374    | 0        | 1,452.68       | 303      | 4.39     |
| 4,000   | 290      | 7,099    | 56.76          | 186      | 5.11     |
| 6,000   | 4,310    | 0        | 5,131.63       | 196      | 0.84     |
| 6,000   | 6,052    | 0        | 1,506.35       | 332      | 4.02     |
| 8,000   | 2,938    | 0        | 4,223.76       | 194      | 0.70     |
| 8,000   | 4,953    | 0        | 2,692.70       | 322      | 1.84     |
| 100,000 | 1,131    | 0        | 2,768.06       | 155      | 0.41     |
| 100,000 | 3,188    | 0        | 3,738.58       | 237      | 0.85     |
| 100,000 | 4,377    | 0        | 2,651.50       | 318      | 1.65     |

### MySQL 压测结果

| 并发数  | 成功操作 | 失败操作 | 吞吐量 (ops/s) | 峰值使用 | 耗时 (s) |
|---------|----------|----------|----------------|----------|----------|
| 1,000   | 2,580    | 0        | 1,174.46       | 114      | 2.20     |
| 2,000   | 3,509    | 411      | 463.08         | 118      | 7.58     |
| 4,000   | 2,855    | 887      | 371.94         | 121      | 7.68     |
| 6,000   | 2,360    | 0        | 1,195.10       | 128      | 1.97     |
| 8,000   | 1,780    | 0        | 1,641.91       | 114      | 1.08     |
| 10,000  | 1,554    | 0        | 1,399.53       | 112      | 1.11     |

### 性能分析

1. **模拟连接池**：在 10,000 并发下仍保持高吞吐（25,571 ops/s），峰值资源使用接近 MaxSize (500)，证明动态扩容机制有效

2. **Redis 实测**：在 100,000 极端并发下依然稳定运行，吞吐量达 2,651 ops/s，且无失败操作，说明池在真实网络环境下表现良好

3. **MySQL 实测**：相比 Redis 吞吐较低（1,399 ops/s），主要受限于 MySQL 连接建立开销和查询延迟，但在高并发下失败率可控

4. **资源利用率**：所有测试中峰值使用均未超过 MaxSize，且大多数场景下维持在中等水平，证明非线性扩缩容算法避免了资源浪费

## 配置说明

```go
type PoolConfig struct {
    MinSize          int64         // 最小连接数
    MaxSize          int64         // 最大连接数
    SurviveTime      time.Duration // 空闲资源存活时间
    MonitorInterval  time.Duration // 监控检查间隔
    IdleBufferFactor float64       // channel 缓冲系数（建议 2.0-3.0）
    MaxRetries       int           // 重连最大重试次数
    RetryInterval    time.Duration // 重试间隔
    ReconnectOnGet   bool          // Get 时是否自动重连失效资源
}
```

建议配置：

- Redis: IdleBufferFactor=3.0, MaxRetries=3
- MySQL: IdleBufferFactor=2.0, MaxRetries=5
- 通用: MinSize=50, MaxSize=500, MonitorInterval=2s

## API 说明

```go
// 创建资源池
func NewPool[T any](config PoolConfig, connControl Conn[T]) *Pool[T]

// 获取资源（支持超时控制）
func (p *Pool[T]) Get(ctx context.Context) (*resource[T], error)

// 归还资源（自动 Reset 或重连）
func (p *Pool[T]) Put(res *resource[T]) error

// 获取统计信息
func (p *Pool[T]) Stats(ctx context.Context) (map[string]int64, error)

// 手动触发扩容
func (p *Pool[T]) TriggerExpand() error

// 手动触发缩容
func (p *Pool[T]) TriggerShrink() error

// 关闭资源池
func (p *Pool[T]) Close()
```

## 注意事项

1. 每个资源应配置为单连接模式（Redis PoolSize=1, MySQL MaxOpenConns=1）
2. 建议根据实际负载调整 IdleBufferFactor，避免 channel 阻塞
3. 启用 ReconnectOnGet 会增加额外开销，仅在连接稳定性要求高时启用
4. 极端高并发场景下可能出现少量失败，建议配合重试机制使用

## License

MIT
