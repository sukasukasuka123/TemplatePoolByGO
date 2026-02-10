# TemplatePoolByGO — High-Performance Generic Resource Pool in Go

## 1. Overview

**TemplatePoolByGO** is a high-performance, generic resource pool implemented in Go.
It is designed around the **Actor model** to manage asynchronous resource **scaling up and down**, combined with a **lock-free waiting queue** to achieve excellent concurrency performance.

The pool supports:

* Resource reuse
* Asynchronous auto-scaling (expand / shrink)
* Health checking
* Timeout control
* Retry and failure handling

It is suitable for pooling **any connection-like resources**, such as database connections, network connections, RPC clients, etc.

---

## 2. Quick Start

### 2.1 Core Pool Configuration

First, define the core configuration of the pool to control capacity, lifetime, and retry strategies:

```go
import (
    "context"
    "time"
    pool "github.com/sukasukasuka123/TemplatePoolByGO"
)

config := PoolConfig{
    MinSize:          50,                     // Minimum number of resources
    MaxSize:          500,                    // Maximum number of resources
    SurviveTime:      180 * time.Second,      // Idle resource survival time
    MonitorInterval:  1 * time.Second,        // Monitoring interval
    IdleBufferFactor: 0.6,                    // Idle buffer factor (stable water level)
    MaxRetries:       3,                      // Retry count for resource creation
    RetryInterval:    200 * time.Millisecond, // Retry interval
    ReconnectOnGet:   false,                  // Recheck health on Get
}
```

---

### 2.2 Implement the Resource Control Interface

The pool relies on a unified **resource control interface** to manage the resource lifecycle:

```go
type ResourceControl[T any] interface {
    Create() (T, error)       // Create a new resource
    Reset(resource T) error   // Reset resource before reuse
    Close(resource T) error   // Close resource
    Ping(resource T) error    // Health check
}
```

Example implementation using a simulated connection:

```go
fc := &FakeConnControl{
    createDelay: 1 * time.Millisecond, // Simulated creation delay
    failRate:    0.05,                 // Simulated failure rate (5%)
}
```

---

### 2.3 Create and Use the Pool

```go
pool := NewPool(config, fc)
defer pool.Close()

ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
defer cancel()

res, err := pool.Get(ctx)
if err != nil {
    panic(err)
}

// Simulate business logic
time.Sleep(5 * time.Millisecond)

// Return resource
if err := pool.Put(res); err != nil {
    panic(err)
}
```

---

## 3. Testing

### 3.1 Core Test Functions

The project provides multiple benchmark and functional tests:

| Test Function                    | Purpose                                      | Key Characteristics                                   |
| -------------------------------- | -------------------------------------------- | ----------------------------------------------------- |
| `BenchmarkStress_GetPut_RealUse` | High-concurrency stress test with real usage | Simulates 5ms usage time, concurrency from 1k to 100k |
| `BenchmarkPool_GetPut`           | Pure scheduling benchmark                    | Tests Get/Put overhead only                           |
| `BenchmarkStress_GetPut`         | Extreme concurrency test                     | No sleep, focuses on stability                        |
| `TestReconnect`                  | Reconnection logic test                      | Verifies `ReconnectOnGet` behavior                    |

---

### 3.2 Metrics Explained

| Metric           | Meaning                           |
| ---------------- | --------------------------------- |
| `total_size`     | Total number of created resources |
| `pool_available` | Idle resources available          |
| `pool_in_use`    | Resources currently in use        |
| `waiting_count`  | Requests waiting in the queue     |
| `expanding`      | Resources being created           |
| `successOps`     | Successful Get+Put operations     |
| `failedOps`      | Failed operations                 |
| `timeoutOps`     | Timeout operations                |
| `throughput`     | Ops per second                    |
| `avgLatency`     | Average latency (ms)              |

---

## 4. Core Design Principles

### 4.1 Get Flow Design

The `Pool.Get` method follows a **four-stage pipeline**:

1. **Fast Path** — non-blocking attempt from idle queue
2. **Slow Path** — lightweight wait + retry
3. **Async Expansion Trigger** — Actor-based scaling
4. **Blocking Wait** — lock-free queue with timeout control

This design keeps the hot path extremely fast while offloading heavy logic to the Actor.

---

### 4.2 Utility Components

#### request_queue (Lock-Free Queue)

* CAS-based lock-free design
* O(1) enqueue / remove / length
* Acts as a pressure buffer under high concurrency

#### closure (Actor Base)

* Encapsulates state and behavior
* All pool size changes happen inside the Actor
* Eliminates shared-state contention

---

### 4.3 Pool Manager (Actor-Based)

The `PoolManager` is responsible for:

* Capacity adjustment (expand / shrink)
* Idle resource cleanup
* Failure retries
* Expansion batching and rate limiting

All state mutations happen **inside the Actor**, guaranteeing thread safety without locks.

---

## 5. Benchmark Results (Real Use)

| Concurrency | Throughput (ops/s) | Avg Latency (ms) | Timeout Rate |
| ----------- | ------------------ | ---------------- | ------------ |
| 1,000       | ~45,000            | ~22              | 0%           |
| 10,000      | ~180,000           | ~55              | <0.5%        |
| 50,000      | ~250,000           | ~200             | <2%          |

### Key Observations

* Throughput plateaus once the pool hits `MaxSize`
* Idle buffer tuning significantly reduces waiting pressure
* Lock-free queue remains stable under extreme load

---

## 6. Future Improvements

* Resource pre-warming on startup
* Adaptive expansion thresholds
* Priority-based resource allocation
* Metrics & alerting integration
* Reset cost optimization

---

## License

MIT
