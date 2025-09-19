# TemplatePoolByGO

A high-performance, generic, asynchronous resource pool library for Go, supporting dynamic scaling and resource health checks. Suitable for managing resources with high creation costs, such as database connections, network connections, or any similar scenarios.

---

## Features

* **Generic Support**: Works with any type of resource (using Go generics with the `any` type).
* **Dynamic Scaling**: Automatically expands or shrinks the pool size based on demand.
* **Resource Lifecycle Management**: Configurable resource lifetime, with automatic cleanup of expired or unavailable resources.
* **Asynchronous Resource Creation**: If the pool is empty and cannot immediately provide a resource, temporary resources are created asynchronously.
* **Thread Safety**: Concurrency safety via channels, mutexes, and atomic operations.
* **Health Checks**: Regularly checks resource health and cleans up failed resources.

---

## Installation

```bash
go get github.com/sukasukasuka123/TemplatePoolByGO@v0.1.0
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
	// Reset resource to initial state
	return nil
}

func (c *MyConnControl) Close(conn MyConn) error {
	// Close resource
	fmt.Println("Close connection:", conn.ID)
	return nil
}

func (c *MyConnControl) Ping(conn MyConn) error {
	// Check resource availability
	return nil
}

func main() {
	control := &MyConnControl{}

	p, err := pool.NewPool[MyConn](
		2,             // Minimum pool size
		10,            // Maximum pool size
		2,             // Expansion step
		1,             // Shrink step
		5*time.Minute, // Resource lifetime
		control,       // Connection controller
	)
	if err != nil {
		panic(err)
	}

	// Acquire resource
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

### Advanced Features

* **Auto Scaling**
  Automatically expands the pool when resources are insufficient, and shrinks it when utilization is low.

* **Resource Monitoring and Cleanup**
  Resources exceeding `surviveTime` or failing Ping checks will be automatically closed.

---

## Notes

* The resource pool supports concurrent usage by multiple goroutines.
* Temporary resources created when the pool is empty do not count toward the pool's maximum capacity.
* Make sure the `Conn[T]` interface methods (`Create`, `Reset`, `Close`, `Ping`) are implemented correctly to ensure proper pool operation.

---

## License

MIT License
