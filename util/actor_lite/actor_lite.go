package actor_lite

type ActorLite[T any] struct {
	data         T
	leaseStation chan func(*T)
}

func NewActorLite[T any](initData T) *ActorLite[T] {
	a := &ActorLite[T]{
		data:         initData,
		leaseStation: make(chan func(*T), 1024),
	}

	go func() {
		// range 会一直阻塞等待，直到 channel 有数据或被 close
		for f := range a.leaseStation {
			f(&a.data)
		}
	}()
	return a
}

// Do 执行指令
func (a *ActorLite[T]) Do(f func(*T)) {
	a.leaseStation <- f
}
