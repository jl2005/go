// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// A Pool is a set of temporary objects that may be individually saved and
// retrieved.
//
// Any item stored in the Pool may be removed automatically at any time without
// notification. If the Pool holds the only reference when this happens, the
// item might be deallocated.
//
// A Pool is safe for use by multiple goroutines simultaneously.
//
// Pool's purpose is to cache allocated but unused items for later reuse,
// relieving pressure on the garbage collector. That is, it makes it easy to
// build efficient, thread-safe free lists. However, it is not suitable for all
// free lists.
//
// An appropriate use of a Pool is to manage a group of temporary items
// silently shared among and potentially reused by concurrent independent
// clients of a package. Pool provides a way to amortize allocation overhead
// across many clients.
//
// An example of good use of a Pool is in the fmt package, which maintains a
// dynamically-sized store of temporary output buffers. The store scales under
// load (when many goroutines are actively printing) and shrinks when
// quiescent.
//
// On the other hand, a free list maintained as part of a short-lived object is
// not a suitable use for a Pool, since the overhead does not amortize well in
// that scenario. It is more efficient to have such objects implement their own
// free list.
//
// A Pool must not be copied after first use.
//
// Pool 是单独被保存和检索的临时对象集合。
//
// 所有存储在Pool中的item可能随时自动删除，而没有任何通知。如果只有Pool中持有item引用，
// 则item可能被释放。
//
// Pool 在多个goroutine中的使用是安全的。
//
// Pool 的目的是缓存已经分配但未使用的 item，以备后用，以减轻垃圾回收的压力。也就是说，
// 它可以轻松构建高效的、线程安全的空闲列表（free list）。但是，它并不适合所有的空闲列表。
//
// Pool 适用于管理一组临时 item，他们可能被包（package）下的不同的客户端（client）悄悄的共享
// 并可能被重用。Pool提供了一种方法，缓解跨多个客户端的分配（allocation）开销。
//
// fmt包中有一个Pool的很好使用的例子，它维护了一个动态大小的临时输出缓冲（buffer）的存储区（store）。
// 这个存储区在很多goroutine活跃打印的时候扩大，在不活跃的时候进行收缩。
//
// 另一方面，对短期对象的一部分而维护的空闲列表是不适合使用Pool的，因为在该场景中开销并不能很好地被分摊。
// 让这些对象实现自己的空闲列表会更高效。
//
// Pool在使用之后不允许拷贝。
type Pool struct {
	noCopy noCopy

	// local 是一个指向数组的指针，数组的每个元素都是poolLocal
	// poolLocal 为每一个核对应的缓存
	// localSize 为数组大小，是CPU 核数，通过runtime.GOMAXPROCS(0)获取。
	local     unsafe.Pointer // local fixed-size per-P pool, actual type is [P]poolLocal
	localSize uintptr        // size of the local array

	// New optionally specifies a function to generate
	// a value when Get would otherwise return nil.
	// It may not be changed concurrently with calls to Get.
	// New 用于在没有可用对象的时候创建一个新的对象
	// 这句话是“它并不会更改Get的并发”？
	New func() interface{}
}

// Local per-P Pool appendix.
// 每个Process对应的空闲池，其中数据分为两部分：
// private 是表示自己私有的内容，其它核是无法抢占，
//         这样可以避免不同核之间的相互抢占PANIC
// shared  是一个数组，当其它核没有空闲数据的时候可以从shared中“偷”数据。
// Mutex   只需要保护shared就好了
type poolLocalInternal struct {
	private interface{}   // Can be used only by the respective P.
	shared  []interface{} // Can be used by any P.
	Mutex                 // Protects shared.
}

type poolLocal struct {
	poolLocalInternal

	// Prevents false sharing on widespread platforms with
	// 128 mod (cache line size) = 0 .
	// 应该是针对特定平台的填充，保证128 Byte对齐。
	// 这里使用Sizeof操作符要比原来（pad [128]byte）的直接设置128会好一些
	pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte
}

// from runtime
func fastrand() uint32

var poolRaceHash [128]uint64

// poolRaceAddr returns an address to use as the synchronization point
// for race detector logic. We don't use the actual pointer stored in x
// directly, for fear of conflicting with other synchronization on that address.
// Instead, we hash the pointer to get an index into poolRaceHash.
// See discussion on golang.org/cl/31589.
func poolRaceAddr(x interface{}) unsafe.Pointer {
	ptr := uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1])
	h := uint32((uint64(uint32(ptr)) * 0x85ebca6b) >> 16)
	return unsafe.Pointer(&poolRaceHash[h%uint32(len(poolRaceHash))])
}

// Put adds x to the pool.
//
// 将 x 放到Pool中，首先尝试放到私有域，然后尝试放到公有域
func (p *Pool) Put(x interface{}) {
	if x == nil {
		return
	}
	if race.Enabled {
		if fastrand()%4 == 0 {
			// Randomly drop x on floor.
			return
		}
		race.ReleaseMerge(poolRaceAddr(x))
		race.Disable()
	}
	l := p.pin()
	if l.private == nil { // 首先检查私有域
		l.private = x
		x = nil
	}
	runtime_procUnpin() // 在pin中会锁定，所以在这里释放
	if x != nil {
		l.Lock()
		l.shared = append(l.shared, x)
		l.Unlock()
	}
	if race.Enabled {
		race.Enable()
	}
}

// Get selects an arbitrary item from the Pool, removes it from the
// Pool, and returns it to the caller.
// Get may choose to ignore the pool and treat it as empty.
// Callers should not assume any relation between values passed to Put and
// the values returned by Get.
//
// If Get would otherwise return nil and p.New is non-nil, Get returns
// the result of calling p.New.
//
// Get 随便选择一个item，从Pool中移除它，并返回给调用者。
// Get 可能会忽略pool，并把他当为空
// 调用者不应该假设放入（Put）和取出（Get）的顺序
//
// 如果Get以其它方式返回nil，但是p.New返回non-nil，则Get返回调用p.New的结果
func (p *Pool) Get() interface{} {
	if race.Enabled {
		race.Disable()
	}
	l := p.pin()
	x := l.private
	l.private = nil
	runtime_procUnpin() //尽快的释放
	if x == nil {
		l.Lock()
		last := len(l.shared) - 1
		if last >= 0 {
			x = l.shared[last]
			l.shared = l.shared[:last]
		}
		l.Unlock()
		if x == nil {
			x = p.getSlow()
		}
	}
	if race.Enabled {
		race.Enable()
		if x != nil {
			race.Acquire(poolRaceAddr(x))
		}
	}
	if x == nil && p.New != nil { // +- 这里是判断是否设置New方法，并不是调用
		x = p.New()
	}
	return x
}

// 尝试从其它P中偷取一个item
func (p *Pool) getSlow() (x interface{}) {
	// See the comment in pin regarding ordering of the loads.
	// 先加载localSize，然后加载local
	size := atomic.LoadUintptr(&p.localSize) // load-acquire
	// +-? 如果这个之间进行多次设置，会不会就导致后面的循环出问题
	local := p.local // load-consume
	// Try to steal one element from other procs.
	pid := runtime_procPin()
	runtime_procUnpin()
	for i := 0; i < int(size); i++ {
		l := indexLocal(local, (pid+i+1)%int(size))
		l.Lock()
		last := len(l.shared) - 1
		if last >= 0 {
			x = l.shared[last]
			l.shared = l.shared[:last]
			l.Unlock()
			break
		}
		l.Unlock()
	}
	return x
}

// pin pins the current goroutine to P, disables preemption and returns poolLocal pool for the P.
// Caller must call runtime_procUnpin() when done with the pool.
//
// 固定当前goroutine到P，禁止抢占，返回P的poolLocal
// 当调用者使用pool之后，**必须**调用runtime_procUnpin()
func (p *Pool) pin() *poolLocal {
	pid := runtime_procPin()
	// In pinSlow we store to localSize and then to local, here we load in opposite order.
	// Since we've disabled preemption, GC cannot happen in between.
	// Thus here we must observe local at least as large localSize.
	// We can observe a newer/larger local, it is fine (we must observe its zero-initialized-ness).
	//
	// 在pinSlow中，我们先设置localSize，然后设置local，这里我们按照相反顺序执行
	// 由于我们已经禁止抢占， 在这个之间不会发生GC。
	// 所以，这里我们不许检查local至少有localSize这么大。
	// 我们可以察觉到一个新的/大的 local，这样没有问题（我们必须察觉到它为空的情况）
	s := atomic.LoadUintptr(&p.localSize) // load-acquire
	l := p.local                          // load-consume
	if uintptr(pid) < s {
		return indexLocal(l, pid)
	}
	return p.pinSlow()
}

// 尝试修复重新分配local
func (p *Pool) pinSlow() *poolLocal {
	// Retry under the mutex.
	// Can not lock the mutex while pinned.
	runtime_procUnpin()
	allPoolsMu.Lock()
	defer allPoolsMu.Unlock()
	pid := runtime_procPin()
	// poolCleanup won't be called while we are pinned.
	// +- 先尝试获取一次，这个习惯好
	s := p.localSize
	l := p.local
	if uintptr(pid) < s {
		return indexLocal(l, pid)
	}
	if p.local == nil {
		// +- 把它放到poolCleanup中清除，而不是立即清除
		allPools = append(allPools, p)
	}
	// If GOMAXPROCS changes between GCs, we re-allocate the array and lose the old one.
	//
	// 如果在GC的过程中，通过 GOMAXPROCS 修改了使用核数，将会重新开辟数组
	// 并丢弃旧的
	size := runtime.GOMAXPROCS(0) //获取使用的核数
	local := make([]poolLocal, size)
	atomic.StorePointer(&p.local, unsafe.Pointer(&local[0])) // store-release
	atomic.StoreUintptr(&p.localSize, uintptr(size))         // store-release
	return &local[pid]
}

// 清理缓冲池
func poolCleanup() {
	// This function is called with the world stopped, at the beginning of a garbage collection.
	// It must not allocate and probably should not call any runtime functions.
	// Defensively zero out everything, 2 reasons:
	// 1. To prevent false retention of whole Pools.
	// 2. If GC happens while a goroutine works with l.shared in Put/Get,
	//    it will retain whole Pool. So next cycle memory consumption would be doubled.
	//
	// 这个函数是在world stopped的时候被调用的，在GC开始的时候
	// 它**不能**分配，也不应该调用runtime functions
	// 防御性的将所有归零，有两个原因：
	// 1. 防止整个Pool的错误保留
	// 2. 如果goroutine正在在Put/Get中使用l.shared，此时发生GC，它将保留整个Pool。
	//    这样下一次循环，内存消耗会增加一倍。
	for i, p := range allPools {
		allPools[i] = nil
		for i := 0; i < int(p.localSize); i++ {
			l := indexLocal(p.local, i)
			l.private = nil
			for j := range l.shared {
				l.shared[j] = nil
			}
			l.shared = nil
		}
		p.local = nil
		p.localSize = 0
	}
	allPools = []*Pool{}
}

var (
	allPoolsMu Mutex
	allPools   []*Pool
)

func init() {
	// 注册PoolClieanup函数，在GC执行开始的的时候调用
	runtime_registerPoolCleanup(poolCleanup)
}

func indexLocal(l unsafe.Pointer, i int) *poolLocal {
	lp := unsafe.Pointer(uintptr(l) + uintptr(i)*unsafe.Sizeof(poolLocal{}))
	return (*poolLocal)(lp)
}

// Implemented in runtime.
func runtime_registerPoolCleanup(cleanup func())
func runtime_procPin() int
func runtime_procUnpin()
