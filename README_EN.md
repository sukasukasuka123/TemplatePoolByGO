# TemplatePoolByGO

A generic, production-ready connection pool for Go, built on a lock-free wait queue and an actor-based pool manager.

```
go get github.com/sukasukasuka123/TemplatePoolByGO@v0.1.7
```

---

## Features

- **Generic** — `Pool[T any]` works with any connection type: gRPC streams, SQL connections, HTTP clients, or anything you wrap.
- **Lock-free wait queue** — waiters queue without mutex contention; CAS-based dequeue with lazy deletion of cancelled nodes.
- **Actor-based pool manager** — all resize decisions run in a single-goroutine event loop (`Closure`), eliminating lock contention on the hot path.
- **Non-linear expansion** — three-phase growth curve (conservative → aggressive → converging) with pressure compensation based on queue depth.
- **Heartbeat / health check** — periodic `Ping` rounds check idle connections and evict unhealthy ones; `OnUnhealthy` callback lets you hook in service-discovery logic.
- **Graceful shutdown** — `Close()` drains the wait queue, stops the manager, and closes all pooled connections cleanly.

---

## Quick Start

### 1. Implement the `Conn[T]` interface

```go
type Conn[T any] interface {
    Create() (T, error)   // allocate a new connection
    Reset(T)  error       // prepare a returned connection for reuse
    Close(T)  error       // release the connection
    Ping(T)   error       // health-check (called by the heartbeat goroutine)
}
```

Example — wrapping a plain TCP connection:

```go
type TCPConn struct{ net.Conn }

type TCPControl struct{ addr string }

func (c *TCPControl) Create() (*TCPConn, error) {
    conn, err := net.Dial("tcp", c.addr)
    if err != nil {
        return nil, err
    }
    return &TCPConn{conn}, nil
}

func (c *TCPControl) Reset(t *TCPConn) error  { return nil }
func (c *TCPControl) Close(t *TCPConn) error  { return t.Close() }
func (c *TCPControl) Ping(t *TCPConn) error {
    // SetDeadline trick: a zero-byte read will succeed on a live conn
    t.SetDeadline(time.Now().Add(time.Second))
    defer t.SetDeadline(time.Time{})
    _, err := t.Read([]byte{})
    if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
        return err
    }
    return nil
}
```

### 2. Configure and create the pool

```go
cfg := pool.PoolConfig{
    MinSize:          5,
    MaxSize:          100,
    IdleBufferFactor: 0.4,   // channel buffer = MaxSize * IdleBufferFactor
    SurviveTime:      30 * time.Minute,
    MonitorInterval:  10 * time.Second,
    MaxWaitQueue:     10000,

    // Heartbeat
    PingInterval: 30 * time.Second,
    OnUnhealthy: func(err error) {
        log.Printf("unhealthy connection evicted: %v", err)
        // trigger service-discovery refresh here if needed
    },

    // Reconnect on Get
    MaxRetries:     3,
    RetryInterval:  time.Second,
    ReconnectOnGet: true,
}

p := pool.NewPool(cfg, &TCPControl{addr: "localhost:9000"})
defer p.Close()
```

### 3. Get / Put

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

res, err := p.Get(ctx)
if err != nil {
    // pool.ErrPoolBusy  — wait queue full, reject immediately
    // context.DeadlineExceeded / context.Canceled — ctx expired while waiting
    return err
}
defer p.Put(res)

// res.Conn is your *TCPConn
res.Conn.Write([]byte("hello"))
```

---

## Configuration Reference

| Field | Type | Default (DefaultPoolConfig) | Description |
|---|---|---|---|
| `MinSize` | `int64` | `5` | Connections created at startup; pool never shrinks below this. |
| `MaxSize` | `int64` | `100` | Hard ceiling on total connections (in-use + idle). |
| `IdleBufferFactor` | `float64` | `1.0` | `cap(resources channel) = MaxSize × factor`. Controls memory for idle slots; does **not** limit `MaxSize`. |
| `SurviveTime` | `time.Duration` | `30m` | Maximum age of a connection before it is eligible for eviction. |
| `MonitorInterval` | `time.Duration` | `10s` | How often the manager runs a shrink check. |
| `MaxWaitQueue` | `int64` | `10000` | Maximum callers that can wait in the lock-free queue before `ErrPoolBusy` is returned. |
| `PingInterval` | `time.Duration` | `30s` | Heartbeat interval. Set to `0` to disable. |
| `OnUnhealthy` | `func(error)` | `nil` | Called each time a `Ping` fails and the connection is evicted. |
| `MaxRetries` | `int` | `3` | Retry attempts when `Create` fails during expansion. |
| `RetryInterval` | `time.Duration` | `1s` | Delay between `Create` retries. |
| `ReconnectOnGet` | `bool` | `true` | If `true`, a failed `Reset` on `Get` triggers one reconnect attempt before returning an error. |

---

## How It Works

### Pool sizing

```
totalSize  = connections currently managed (in-use + idle)
bufferSize = cap(resources channel) = MaxSize × IdleBufferFactor

totalSize can reach MaxSize regardless of bufferSize.
bufferSize only controls how many idle connections sit in the channel at once.
```

### Expansion — three-phase curve

The pool manager calculates `expandSize` based on how full the pool is relative to `[MinSize, MaxSize]`:

```
usageRate = (totalSize - MinSize) / (MaxSize - MinSize)

< 20%  → conservative: +15 per round
20–75% → aggressive:   +remaining/2 per round
> 75%  → converging:   +remaining/8 per round

pressureStep = waitingCount / 3   (/ 2 if waitingCount > 100)
finalStep    = max(curveStep, pressureStep), capped at MaxSize/3
```

Expansion is triggered when:
- there are waiters **and** `totalSize < MaxSize`, or
- the idle buffer utilisation drops below 30% (most connections in use).

### Shrink

Shrink fires when idle buffer utilisation exceeds 90%, no one is waiting, and `totalSize > MinSize`. Each shrink round removes at most 20% of the surplus above `MinSize`.

### Heartbeat

Every `PingInterval` a background goroutine pulls up to 5 idle connections from the channel, calls `Ping` on each, then:
- **healthy** → returned to the channel (or handed directly to a new waiter).
- **unhealthy** → closed, `totalSize` decremented, manager asked to refill, `OnUnhealthy` called.

The round is skipped entirely if any waiter is in the queue, so heartbeats never starve active callers.

### Wait queue

`Get` enqueues a `LockFreeWaiter` backed by a buffered `chan T`. When a connection becomes available (`Put`, expansion, or heartbeat), `TryDequeue` does a CAS to hand it directly to the first non-cancelled waiter, bypassing the channel entirely. Cancelled waiters are lazily removed during the next `TryDequeue` pass.

---

## Errors

| Error | When |
|---|---|
| `pool.ErrPoolBusy` | `waitQueue.Len() >= MaxWaitQueue` at the time of `Get`. |
| `context.DeadlineExceeded` / `context.Canceled` | The caller's context expired while waiting in the queue. |
| `"pool closed"` | `Get` was waiting when `Close()` drained the queue. |

---

## Stats

```go
stats, _ := p.Stats(context.Background())
// map[string]int64{
//   "total_size":     current total connections,
//   "pool_available": idle connections in the channel,
//   "pool_in_use":    connections currently checked out,
//   "waiting_count":  callers blocked in the wait queue,
//   "expanding":      connections being created right now,
//   "buffer_cap":     cap(resources channel),
// }
```

---

## Integrating `OnUnhealthy` with Service Discovery

A common pattern when using the pool in front of a remote service (e.g. gRPC):

```go
OnUnhealthy: func(err error) {
    // The Ping implementation can embed the failure reason.
    // Distinguish connection-level failure from stream-level failure
    // and act accordingly.
    if isConnFailure(err) {
        // Signal your service registry to re-resolve the address
        // and rebuild the pool for this target.
        serviceDiscovery.TriggerRefresh(addr)
    }
    // Stream-level failures: the pool's own checkAndAdjust
    // will replenish — no external action needed.
},
```

The callback runs in the heartbeat goroutine, so keep it non-blocking (spawn a goroutine or send to a channel if the work is heavy).

---

## License

MIT