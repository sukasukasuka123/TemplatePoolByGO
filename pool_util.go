package pool

import (
	"sync"
	"time"
)

// 定义一个必须实现的接口
type Conn[T any] interface {
	Reset(T) error
	Close(T) error
	Create() (T, error)
	Ping(T) error
}

type resource[T any] struct {
	ID         string    // 资源ID
	createTime time.Time // 创建时间
	updateTime time.Time // 更新时间
	Conn       T         // 资源本身
}

type Pool[T any] struct {
	// 资源池
	Resources        chan *resource[T]
	isChangeSize     int32         // 改动：使用 int32 搭配 atomic
	minisize         int64         // 最小资源数
	maxsize          int64         // 最大资源数
	nowsize          int64         // 当前资源数(包括池内池外)
	nowPoolsize      int64         // 当前资源池大小
	expandSizeAtOnce int64         // 每次扩容的资源数量
	shrinkSizeAtOnce int64         // 每次缩容的资源数量
	surviveTime      time.Duration // 一个资源的存活时间

	//资源池相关配置
	// 资源控制函数
	connControl Conn[T] //连接的一些基本函数

	// 相关锁
	ConnLock           sync.Mutex    // 创建连接锁
	createResourceLock sync.Mutex    // 创建临时资源锁
	mu                 sync.RWMutex  // 对资源池大小处理的锁（雾）
	stopChan           chan struct{} // 用于停止监控协程
	once               sync.Once     // 确保协程只启动一次
	// 相关通道
	expandChan chan struct{} // 扩容信号通道
	shrinkChan chan struct{} // 缩容信号通道
}
