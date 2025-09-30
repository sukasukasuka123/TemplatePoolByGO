(docs/now.png)

# TemplatePoolByGO

A high-performance, generic, and asynchronous Go resource pool library. It supports dynamic scaling, shrinking, and resource health checks—perfect for managing database connections, network connections, or any resource with a high creation cost.

---

## Features

* **Generics support**: Works with any resource type using Go generics (`any`).
* **Dynamic scaling**: Automatically expands or shrinks the pool size based on demand.
* **Resource lifecycle management**: Configurable resource lifetime with automatic cleanup of expired or invalid resources.
* **Asynchronous resource creation**: When the pool is empty and cannot immediately provide resources, temporary resources can be created asynchronously.
* **Thread safety**: Concurrently safe with low overhead, implemented using channels, CAS, and atomic operations.
* **Health checks**: Periodically validates resources and cleans up failures.

---

## Installation

```bash
go get github.com/sukasukasuka123/TemplatePoolByGO@v0.1.0
```

Latest version:

```bash
go get github.com/sukasukasuka123/TemplatePoolByGO@v0.1.2.1
```

---

## Usage

```go
package main

import (
	"context"
	"fmt"
	"time"

	pool "github.com/sukasukasuka123/TemplatePoolByGO"
)

type MyConn struct {
	ID int
}

// Implement the Conn interface
type MyConnControl struct{}

func (c *MyConnControl) Create() (MyConn, error) {
	return MyConn{ID: int(time.Now().UnixNano())}, nil
}

func (c *MyConnControl) Reset(conn MyConn) error {
	// Reset resource to its initial state
	return nil
}

func (c *MyConnControl) Close(conn MyConn) error {
	// Close resource
	fmt.Println("Closing connection:", conn.ID)
	return nil
}

func (c *MyConnControl) Ping(conn MyConn) error {
	// Check if the resource is alive
	return nil
}

func main() {
	control := &MyConnControl{}

	p, err := pool.NewPool[MyConn](
		2,             // Minimum pool size
		10,            // Maximum pool size
		2,             // Expand step
		1,             // Shrink step
		5*time.Minute, // Resource lifetime
		control,       // Resource controller
	)
	if err != nil {
		panic(err)
	}

	// Acquire a resource
	ctx := context.Background()
	res, err := p.GetResource(ctx, ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println("Acquired resource:", res.Conn.ID)

	// Return resource
	p.PutResource(ctx, res)
}
```

---

### Advanced Features

* **Auto-scaling**
  Automatically scales up when resources are insufficient, and scales down when utilization is low.

* **Resource monitoring and cleanup**
  Resources exceeding `surviveTime` or failing the `Ping` check are automatically closed.

---

## Notes

* The pool is safe for use across multiple goroutines.
* Temporary resources created when the pool is empty are **not counted** toward the maximum pool size.
* Ensure the `Conn[T]` interface methods (`Create`, `Reset`, `Close`, `Ping`) are properly implemented to guarantee correct behavior.

---

## License

MIT License

---


# TemplatePoolByGO


一个高性能、泛型、异步的 Go 资源池库，支持动态扩容、缩容和资源健康检查，适合数据库连接、网络连接或任何创建代价高的资源管理。

---

## 特性

* **泛型支持**：支持任意类型的资源（Go 泛型 `any` 类型）。
* **动态扩缩容**：根据需求自动扩展或缩减池大小。
* **资源生命周期管理**：可配置资源存活时间，自动清理过期或不可用资源。
* **异步资源创建**：当池为空且无法立即提供时，可异步创建临时资源。
* **线程安全**：通过管道、cas和原子操作实现并发安全且资源消耗少。
* **健康检查**：定期检查资源健康状况并清理失效资源。

---

## 安装

```bash
go get github.com/sukasukasuka123/TemplatePoolByGO@v0.1.0
```
最新版本：
```bash
go get github.com/sukasukasuka123/TemplatePoolByGO@v0.1.2.1
```
---

## 使用方法


```go
package main

import (
	"context"
	"fmt"
	"time"

	pool "github.com/sukasukasuka123/TemplatePoolByGO"
)

type MyConn struct {
	ID int
}

// 实现 Conn 接口
type MyConnControl struct{}

func (c *MyConnControl) Create() (MyConn, error) {
	return MyConn{ID: int(time.Now().UnixNano())}, nil
}

func (c *MyConnControl) Reset(conn MyConn) error {
	// 重置资源到初始状态
	return nil
}

func (c *MyConnControl) Close(conn MyConn) error {
	// 关闭资源
	fmt.Println("关闭连接:", conn.ID)
	return nil
}

func (c *MyConnControl) Ping(conn MyConn) error {
	// 检查资源是否可用
	return nil
}

func main() {
	control := &MyConnControl{}

	p, err := pool.NewPool[MyConn](
		2,             // 最小池大小
		10,            // 最大池大小
		2,             // 每次扩容数量
		1,             // 每次缩容数量
		5*time.Minute, // 资源存活时间
		control,       // 连接控制器
	)
	if err != nil {
		panic(err)
	}

	// 获取资源
	ctx := context.Background()
	res, err := p.GetResource(ctx, ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println("获取到资源:", res.Conn.ID)

	// 归还资源
	p.PutResource(ctx, res)
}
```

### 3. 高级特性

* **自动扩缩容**
  当资源不足时自动扩容，当资源利用率低时自动缩容。

* **资源监控与清理**
  超过 `surviveTime` 或 Ping 检查失败的资源会自动关闭。

---

## 注意事项

* 资源池支持多 goroutine 并发使用。
* 当池为空时创建的临时资源不会计入池的最大容量。
* 确保 `Conn[T]` 接口方法 (`Create`, `Reset`, `Close`, `Ping`) 正确实现，以保证资源池正常工作。

---

## 许可证

MIT License



