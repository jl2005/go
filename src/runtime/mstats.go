// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Memory statistics

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// Statistics.
// If you edit this structure, also edit type MemStats below.
// Their layouts must match exactly.
//
// For detailed descriptions see the documentation for MemStats.
// Fields that differ from MemStats are further documented here.
//
// Many of these fields are updated on the fly, while others are only
// updated when updatememstats is called.
type mstats struct {
	// General statistics.
	alloc       uint64 // bytes allocated and not yet freed
	total_alloc uint64 // bytes allocated (even if freed)
	sys         uint64 // bytes obtained from system (should be sum of xxx_sys below, no locking, approximate)
	nlookup     uint64 // number of pointer lookups
	nmalloc     uint64 // number of mallocs
	nfree       uint64 // number of frees

	// Statistics about malloc heap.
	// Protected by mheap.lock
	//
	// Like MemStats, heap_sys and heap_inuse do not count memory
	// in manually-managed spans.
	heap_alloc    uint64 // bytes allocated and not yet freed (same as alloc above)
	heap_sys      uint64 // virtual address space obtained from system for GC'd heap
	heap_idle     uint64 // bytes in idle spans
	heap_inuse    uint64 // bytes in _MSpanInUse spans
	heap_released uint64 // bytes released to the os
	heap_objects  uint64 // total number of allocated objects

	// TODO(austin): heap_released is both useless and inaccurate
	// in its current form. It's useless because, from the user's
	// and OS's perspectives, there's no difference between a page
	// that has not yet been faulted in and a page that has been
	// released back to the OS. We could fix this by considering
	// newly mapped spans to be "released". It's inaccurate
	// because when we split a large span for allocation, we
	// "unrelease" all pages in the large span and not just the
	// ones we split off for use. This is trickier to fix because
	// we currently don't know which pages of a span we've
	// released. We could fix it by separating "free" and
	// "released" spans, but then we have to allocate from runs of
	// free and released spans.

	// Statistics about allocation of low-level fixed-size structures.
	// Protected by FixAlloc locks.
	stacks_inuse uint64 // bytes in manually-managed stack spans
	stacks_sys   uint64 // only counts newosproc0 stack in mstats; differs from MemStats.StackSys
	mspan_inuse  uint64 // mspan structures
	mspan_sys    uint64
	mcache_inuse uint64 // mcache structures
	mcache_sys   uint64
	buckhash_sys uint64 // profiling bucket hash table
	gc_sys       uint64
	other_sys    uint64

	// Statistics about garbage collector.
	// Protected by mheap or stopping the world during GC.
	next_gc         uint64 // goal heap_live for when next GC ends; ^0 if disabled
	last_gc_unix    uint64 // last gc (in unix time)
	pause_total_ns  uint64
	pause_ns        [256]uint64 // circular buffer of recent gc pause lengths
	pause_end       [256]uint64 // circular buffer of recent gc end times (nanoseconds since 1970)
	numgc           uint32
	numforcedgc     uint32  // number of user-forced GCs
	gc_cpu_fraction float64 // fraction of CPU time used by GC
	enablegc        bool
	debuggc         bool

	// Statistics about allocation size classes.

	by_size [_NumSizeClasses]struct {
		size    uint32
		nmalloc uint64
		nfree   uint64
	}

	// Statistics below here are not exported to MemStats directly.

	last_gc_nanotime uint64 // last gc (monotonic time)
	tinyallocs       uint64 // number of tiny allocations that didn't cause actual allocation; not exported to go directly

	// triggerRatio is the heap growth ratio that triggers marking.
	//
	// E.g., if this is 0.6, then GC should start when the live
	// heap has reached 1.6 times the heap size marked by the
	// previous cycle. This should be ≤ GOGC/100 so the trigger
	// heap size is less than the goal heap size. This is set
	// during mark termination for the next cycle's trigger.
	triggerRatio float64

	// gc_trigger is the heap size that triggers marking.
	//
	// When heap_live ≥ gc_trigger, the mark phase will start.
	// This is also the heap size by which proportional sweeping
	// must be complete.
	//
	// This is computed from triggerRatio during mark termination
	// for the next cycle's trigger.
	gc_trigger uint64

	// heap_live is the number of bytes considered live by the GC.
	// That is: retained by the most recent GC plus allocated
	// since then. heap_live <= heap_alloc, since heap_alloc
	// includes unmarked objects that have not yet been swept (and
	// hence goes up as we allocate and down as we sweep) while
	// heap_live excludes these objects (and hence only goes up
	// between GCs).
	//
	// This is updated atomically without locking. To reduce
	// contention, this is updated only when obtaining a span from
	// an mcentral and at this point it counts all of the
	// unallocated slots in that span (which will be allocated
	// before that mcache obtains another span from that
	// mcentral). Hence, it slightly overestimates the "true" live
	// heap size. It's better to overestimate than to
	// underestimate because 1) this triggers the GC earlier than
	// necessary rather than potentially too late and 2) this
	// leads to a conservative GC rate rather than a GC rate that
	// is potentially too low.
	//
	// Reads should likewise be atomic (or during STW).
	//
	// Whenever this is updated, call traceHeapAlloc() and
	// gcController.revise().
	heap_live uint64

	// heap_scan is the number of bytes of "scannable" heap. This
	// is the live heap (as counted by heap_live), but omitting
	// no-scan objects and no-scan tails of objects.
	//
	// Whenever this is updated, call gcController.revise().
	heap_scan uint64

	// heap_marked is the number of bytes marked by the previous
	// GC. After mark termination, heap_live == heap_marked, but
	// unlike heap_live, heap_marked does not change until the
	// next mark termination.
	heap_marked uint64
}

var memstats mstats

// A MemStats records statistics about the memory allocator.
// MemStats 记录内存分配的统计信息
type MemStats struct {
	// General statistics.

	// Alloc is bytes of allocated heap objects.
	//
	// This is the same as HeapAlloc (see below).
	//
	// Alloc 是heap中分配的字节数，同HeapAlloc
	Alloc uint64

	// TotalAlloc is cumulative bytes allocated for heap objects.
	//
	// TotalAlloc increases as heap objects are allocated, but
	// unlike Alloc and HeapAlloc, it does not decrease when
	// objects are freed.
	//
	// TotalAlloc 为分配的heap对象的累积字节数
	//
	// TotalAlloc 在分配heap对象的时候进行增加，但是与Alloc和
	// HeapAlloc不同，在对象释放的时候不会减少。
	TotalAlloc uint64

	// Sys is the total bytes of memory obtained from the OS.
	//
	// Sys is the sum of the XSys fields below. Sys measures the
	// virtual address space reserved by the Go runtime for the
	// heap, stacks, and other internal data structures. It's
	// likely that not all of the virtual address space is backed
	// by physical memory at any given moment, though in general
	// it all was at some point.
	//
	// Sys 是从OS获取的总共内存数
	//
	// Sys 是下面 XSys 字段的总和。Sys为Go 运行时堆、堆栈和其它
	// 内部数据结构的保留虚拟地址空间。可能在给定的时刻，并非所有的
	// 虚拟内存都管理物理内存，尽管可能在某一个时刻会关联。
	Sys uint64

	// Lookups is the number of pointer lookups performed by the
	// runtime.
	//
	// This is primarily useful for debugging runtime internals.
	//
	// Lookups 是由运行时执行的指针查找
	//
	// 这主要用于调试运行时内部。
	Lookups uint64

	// Mallocs is the cumulative count of heap objects allocated.
	// The number of live objects is Mallocs - Frees.
	//
	// Mallocs 是申请的heap对象的累计数。
	// 存活的对象数为 Mallocs - Frees
	Mallocs uint64

	// Frees is the cumulative count of heap objects freed.
	//
	// Frees 是释放的heap对象累计数
	Frees uint64

	// Heap memory statistics.
	//
	// Interpreting the heap statistics requires some knowledge of
	// how Go organizes memory. Go divides the virtual address
	// space of the heap into "spans", which are contiguous
	// regions of memory 8K or larger. A span may be in one of
	// three states:
	//
	// An "idle" span contains no objects or other data. The
	// physical memory backing an idle span can be released back
	// to the OS (but the virtual address space never is), or it
	// can be converted into an "in use" or "stack" span.
	//
	// An "in use" span contains at least one heap object and may
	// have free space available to allocate more heap objects.
	//
	// A "stack" span is used for goroutine stacks. Stack spans
	// are not considered part of the heap. A span can change
	// between heap and stack memory; it is never used for both
	// simultaneously.
	//
	// 堆栈内存统计
	//
	// 解释堆统计信息需要了解Go如何组织内存。
	// Go将堆的虚拟地址空间划分为"spans"，它们是8K或更大内存的连续区域。
	// span 可能处于以下三种状态之一：
	//
	// "idle" span不包含对象或数据。idle 的span可以归还给操作系统
	// (但是虚拟内存永远不会)，或转换为 "in use"或"stack"
	//
	// "in use" span 至少包含一个对象，并且可以具有可用空间来分配更多堆对象。
	//
	// "stack" span 用于协程堆栈。stack span 不作为堆的一部分。
	// span 可以在堆内存和栈内存之间切换；但是它会同时使用两者。

	// HeapAlloc is bytes of allocated heap objects.
	//
	// "Allocated" heap objects include all reachable objects, as
	// well as unreachable objects that the garbage collector has
	// not yet freed. Specifically, HeapAlloc increases as heap
	// objects are allocated and decreases as the heap is swept
	// and unreachable objects are freed. Sweeping occurs
	// incrementally between GC cycles, so these two processes
	// occur simultaneously, and as a result HeapAlloc tends to
	// change smoothly (in contrast with the sawtooth that is
	// typical of stop-the-world garbage collectors).
	//
	// HeapAlloc 是分配堆对象的字节数
	//
	// "Allocated" 堆对象包含所有可达的对象，同时也包含GC 还没有
	// 回收的不可达的对象。具体来说，HeapAlloc 随着堆对象的分配
	// 而增加，并随着堆扫描和释放而减少。在GC循环的时候会有扫描，
	// 所以这两个过程同时发生，因此HeapAlloc趋于平滑的变换（与
	// stop-the-world 的垃圾收集器的典型锯齿形成对比）。
	HeapAlloc uint64

	// HeapSys is bytes of heap memory obtained from the OS.
	//
	// HeapSys measures the amount of virtual address space
	// reserved for the heap. This includes virtual address space
	// that has been reserved but not yet used, which consumes no
	// physical memory, but tends to be small, as well as virtual
	// address space for which the physical memory has been
	// returned to the OS after it became unused (see HeapReleased
	// for a measure of the latter).
	//
	// HeapSys estimates the largest size the heap has had.
	//
	// HeapSys 是从操作系统获取内存的字节数
	//
	// HeapSys 为堆中保留的虚拟地址。这包括已经保留但尚未使用
	// 的虚拟地址空间，它不消耗物理内存，但是往往很小，以及已经归还
	// 物理内存并且标记为未使用的虚拟地址内存（参照后文的HeapReleased）。
	//
	// HeapSys 用来估计对的最大虚拟地址空间。
	HeapSys uint64

	// HeapIdle is bytes in idle (unused) spans.
	//
	// Idle spans have no objects in them. These spans could be
	// (and may already have been) returned to the OS, or they can
	// be reused for heap allocations, or they can be reused as
	// stack memory.
	//
	// HeapIdle minus HeapReleased estimates the amount of memory
	// that could be returned to the OS, but is being retained by
	// the runtime so it can grow the heap without requesting more
	// memory from the OS. If this difference is significantly
	// larger than the heap size, it indicates there was a recent
	// transient spike in live heap size.
	//
	// HeapIdle 是空闲（未使用）span 的字节数
	// Idle span 中没有对象。这些span可能（或者已经）归还给OS，
	// 或者供堆分配重用，或被用作栈内存。
	//
	// HeapIdle 减 HeapReleased 用来估算多少内存可以归还给OS，
	// 由于它已经被runtime持有，所以可以在不从OS申请的时候就可以增长堆。
	// 如果此差异明显大于堆大小，则表示实时堆大小中存在最近的瞬态峰值。
	HeapIdle uint64

	// HeapInuse is bytes in in-use spans.
	//
	// In-use spans have at least one object in them. These spans
	// can only be used for other objects of roughly the same
	// size.
	//
	// HeapInuse minus HeapAlloc esimates the amount of memory
	// that has been dedicated to particular size classes, but is
	// not currently being used. This is an upper bound on
	// fragmentation, but in general this memory can be reused
	// efficiently.
	//
	// HeapInuse 是正在使用的span的字节数
	//
	// in-use span 至少包含一个对象。这个span可以直接被其它有
	// 相同大小的对象直接使用。
	//
	// HeapInuse 减 HeapAlloc 可以用来评估专用于特定大小分类的
	// 内存量。这个是内存碎片的上限，但是它通常能you效的重用。
	HeapInuse uint64

	// HeapReleased is bytes of physical memory returned to the OS.
	//
	// This counts heap memory from idle spans that was returned
	// to the OS and has not yet been reacquired for the heap.
	//
	// HeapReleaased 是归还给OS的物理内存字节数.
	//
	// 它是空闲span中归还给OS，但是还没有被堆重用的内存。
	HeapReleased uint64

	// HeapObjects is the number of allocated heap objects.
	//
	// Like HeapAlloc, this increases as objects are allocated and
	// decreases as the heap is swept and unreachable objects are
	// freed.
	//
	// HeapObjects 是已经分配的堆对象的个数
	//
	// 类似于HeapAlloc， 它随着分配对象而增长，随着堆的清扫
	// 和不可达对象的释放而降低。
	HeapObjects uint64

	// Stack memory statistics.
	//
	// Stacks are not considered part of the heap, but the runtime
	// can reuse a span of heap memory for stack memory, and
	// vice-versa.
	//
	// 栈内存统计
	//
	// 栈不是堆的一部分，但是runtime可以重用一个堆内存span
	// 指向栈内存，反之亦然。

	// StackInuse is bytes in stack spans.
	//
	// In-use stack spans have at least one stack in them. These
	// spans can only be used for other stacks of the same size.
	//
	// There is no StackIdle because unused stack spans are
	// returned to the heap (and hence counted toward HeapIdle).
	//
	// StackInuse 是栈span的字节数
	//
	// 正在使用的栈span中至少有一个栈。这些span只有被有相同大小
	// 的其它栈所使用。
	//
	// 没有StackIdle,因为没有使用栈span会归还到堆（记录在HeapIdle中）
	StackInuse uint64

	// StackSys is bytes of stack memory obtained from the OS.
	//
	// StackSys is StackInuse, plus any memory obtained directly
	// from the OS for OS thread stacks (which should be minimal).
	//
	// StackSys 是从OS申请的栈内存字节数。
	//
	// StackSys 是 StackInuse 加上其OS线程堆栈的内存（这可能很小）
	StackSys uint64

	// Off-heap memory statistics.
	//
	// The following statistics measure runtime-internal
	// structures that are not allocated from heap memory (usually
	// because they are part of implementing the heap). Unlike
	// heap or stack memory, any memory allocated to these
	// structures is dedicated to these structures.
	//
	// These are primarily useful for debugging runtime memory
	// overheads.
	//
	//  堆外内存统计
	//
	// 以下统计从运行时内部结构，它们不会从堆中分配（通常它们是实现
	// 堆的一部分）。与堆或栈内存不同，分配给这些结构的内存专用于这些结构。
	//
	// 这些主要用于调试运行时内存开销。

	// MSpanInuse is bytes of allocated mspan structures.
	// MSpanInuse 是分配span结构体的字节数
	MSpanInuse uint64

	// MSpanSys is bytes of memory obtained from the OS for mspan
	// structures.
	// MSpanSys 是从OS获取后用做mspan的内存字节数
	MSpanSys uint64

	// MCacheInuse is bytes of allocated mcache structures.
	// MCacheInuse 是mcache结构体的字节数
	MCacheInuse uint64

	// MCacheSys is bytes of memory obtained from the OS for
	// mcache structures.
	// MCacheSys 是从OS获取，用作mcache结构体的内存字节数
	MCacheSys uint64

	// BuckHashSys is bytes of memory in profiling bucket hash tables.
	// BuckHashSys 是分析存储桶哈希表中的内存字节数。
	BuckHashSys uint64

	// GCSys is bytes of memory in garbage collection metadata.
	// GCSys 是存储垃圾收集元数据的内存字节数
	GCSys uint64

	// OtherSys is bytes of memory in miscellaneous off-heap
	// runtime allocations.
	// OtherSys 是各种堆外运行时分配中的内存字节。
	OtherSys uint64

	// Garbage collector statistics.
	// 垃圾回收统计

	// NextGC is the target heap size of the next GC cycle.
	//
	// The garbage collector's goal is to keep HeapAlloc ≤ NextGC.
	// At the end of each GC cycle, the target for the next cycle
	// is computed based on the amount of reachable data and the
	// value of GOGC.
	//
	// NextGC 是下一次GC循环的目标堆栈大小
	//
	// 垃圾回收的目标是保证HeapAlloc ≤ NextGC。
	// 在每次GC循环中，根据可达量和GOGC值计算下一个周期的目标。
	NextGC uint64

	// LastGC is the time the last garbage collection finished, as
	// nanoseconds since 1970 (the UNIX epoch).
	//
	// LastGC 是最后一次垃圾回收结束的时间，使用从1970年到现在的纳秒
	// (UNIX 纪元）
	LastGC uint64

	// PauseTotalNs is the cumulative nanoseconds in GC
	// stop-the-world pauses since the program started.
	//
	// During a stop-the-world pause, all goroutines are paused
	// and only the garbage collector can run.
	//
	// PauseTotalNs 是从程序运行开始，GC stop-the-world 的累积纳秒。
	//
	// 由于stop-the-world暂停，所有协程会暂停，只有垃圾回收可以运行。
	PauseTotalNs uint64

	// PauseNs is a circular buffer of recent GC stop-the-world
	// pause times in nanoseconds.
	//
	// The most recent pause is at PauseNs[(NumGC+255)%256]. In
	// general, PauseNs[N%256] records the time paused in the most
	// recent N%256th GC cycle. There may be multiple pauses per
	// GC cycle; this is the sum of all pauses during a cycle.
	//
	// PauseNs 是一个环形缓冲，记录最近的GC stop-the-world暂停时间
	// 单位是纳秒
	//
	// 最近的暂停是在PauseNs[(NumGC+255)%256]。PauseNs[N%256]记录
	// 在最近的 N%256 次的GC循环暂停时间。每次GC循环中可能会有多次停留。
	// 这是一个周期内所有暂停的总和。
	PauseNs [256]uint64

	// PauseEnd is a circular buffer of recent GC pause end times,
	// as nanoseconds since 1970 (the UNIX epoch).
	//
	// This buffer is filled the same way as PauseNs. There may be
	// multiple pauses per GC cycle; this records the end of the
	// last pause in a cycle.
	//
	// PauseEnd 是一个循环缓冲，它记录GC暂停的结束时间，使用
	// 从1970年到现在的纳秒（UNIX纪元）
	//
	// 它使用同PauseNs相同的方式进行填充。在每次GC循环中可能有
	// 多次暂停，它记录循环中最后一次的暂停时间。
	PauseEnd [256]uint64

	// NumGC is the number of completed GC cycles.
	// NumGC 是完成GC循环的次数
	NumGC uint32

	// NumForcedGC is the number of GC cycles that were forced by
	// the application calling the GC function.
	//
	// NumForcedGC 是应用程序调用GC函数的次数
	NumForcedGC uint32

	// GCCPUFraction is the fraction of this program's available
	// CPU time used by the GC since the program started.
	//
	// GCCPUFraction is expressed as a number between 0 and 1,
	// where 0 means GC has consumed none of this program's CPU. A
	// program's available CPU time is defined as the integral of
	// GOMAXPROCS since the program started. That is, if
	// GOMAXPROCS is 2 and a program has been running for 10
	// seconds, its "available CPU" is 20 seconds. GCCPUFraction
	// does not include CPU time used for write barrier activity.
	//
	// This is the same as the fraction of CPU reported by
	// GODEBUG=gctrace=1.
	//
	// GCCPUFraction 是从程序启动开始，GC占用整个程序CPU运行时间
	// 的占比。
	//
	// GCCPUFraction 是0到1之间的数，0表示GC没有消耗程序的CPU。
	// 一个程序的CPU可用时间定义为从程序启动以来GOMAXPROCS 的
	// 积分。如果GOMAXPROCS是2，程序已经运行了10秒，则可用CPU
	// 为20秒。GCCPUFraction 不包含用于写屏障的CPU时间。
	//
	// 这与 GODEBUG=gctrace=1 报告的分数相同
	GCCPUFraction float64

	// EnableGC indicates that GC is enabled. It is always true,
	// even if GOGC=off.
	// EnableGC 表示GC是启用的，它总是true，即使GOGC=off
	EnableGC bool

	// DebugGC is currently unused.
	// DebugGC 当前没有使用
	DebugGC bool

	// BySize reports per-size class allocation statistics.
	//
	// BySize[N] gives statistics for allocations of size S where
	// BySize[N-1].Size < S ≤ BySize[N].Size.
	//
	// This does not report allocations larger than BySize[60].Size.
	//
	// BySize 表示每种size-class分配的统计
	//
	// BySize[N] 给出大小为S 的分配统计，其中
	// BySize[N-1].Size < S ≤ BySize[N].Size
	//
	// 此处没有报告超过BySize[60].Size的分配统计
	BySize [61]struct {
		// Size is the maximum byte size of an object in this
		// size class.
		// Size 在这个size-class中的最大字节大小
		Size uint32

		// Mallocs is the cumulative count of heap objects
		// allocated in this size class. The cumulative bytes
		// of allocation is Size*Mallocs. The number of live
		// objects in this size class is Mallocs - Frees.
		//
		// Mallocs 是size class的堆栈对象的累积分配数。
		// 分配的累积自己数为 Size * Mallocs。这个size class
		// 中的存活对象数据是 Malloc-Frees
		Mallocs uint64

		// Frees is the cumulative count of heap objects freed
		// in this size class.
		// Frees 是这个size-class的累积释放对象数。
		Frees uint64
	}
}

// Size of the trailing by_size array differs between mstats and MemStats,
// and all data after by_size is local to runtime, not exported.
// NumSizeClasses was changed, but we cannot change MemStats because of backward compatibility.
// sizeof_C_MStats is the size of the prefix of mstats that
// corresponds to MemStats. It should match Sizeof(MemStats{}).
var sizeof_C_MStats = unsafe.Offsetof(memstats.by_size) + 61*unsafe.Sizeof(memstats.by_size[0])

func init() {
	var memStats MemStats
	if sizeof_C_MStats != unsafe.Sizeof(memStats) {
		println(sizeof_C_MStats, unsafe.Sizeof(memStats))
		throw("MStats vs MemStatsType size mismatch")
	}

	if unsafe.Offsetof(memstats.heap_live)%8 != 0 {
		println(unsafe.Offsetof(memstats.heap_live))
		throw("memstats.heap_live not aligned to 8 bytes")
	}
}

// ReadMemStats populates m with memory allocator statistics.
//
// The returned memory allocator statistics are up to date as of the
// call to ReadMemStats. This is in contrast with a heap profile,
// which is a snapshot as of the most recently completed garbage
// collection cycle.
//
// ReadMemStats 使用内存分配器统计信息填充 m。
//
// 从 ReadMemStats 调用开始，返回的内存分配器统计信息是最新的。
// 这与heap profile 形成对照，heap profile 是最近完成的垃圾收集周期的快照。
func ReadMemStats(m *MemStats) {
	stopTheWorld("read mem stats")

	systemstack(func() {
		readmemstats_m(m)
	})

	startTheWorld()
}

func readmemstats_m(stats *MemStats) {
	updatememstats()

	// The size of the trailing by_size array differs between
	// mstats and MemStats. NumSizeClasses was changed, but we
	// cannot change MemStats because of backward compatibility.
	memmove(unsafe.Pointer(stats), unsafe.Pointer(&memstats), sizeof_C_MStats)

	// memstats.stacks_sys is only memory mapped directly for OS stacks.
	// Add in heap-allocated stack memory for user consumption.
	stats.StackSys += stats.StackInuse
}

//go:linkname readGCStats runtime/debug.readGCStats
func readGCStats(pauses *[]uint64) {
	systemstack(func() {
		readGCStats_m(pauses)
	})
}

func readGCStats_m(pauses *[]uint64) {
	p := *pauses
	// Calling code in runtime/debug should make the slice large enough.
	if cap(p) < len(memstats.pause_ns)+3 {
		throw("short slice passed to readGCStats")
	}

	// Pass back: pauses, pause ends, last gc (absolute time), number of gc, total pause ns.
	lock(&mheap_.lock)

	n := memstats.numgc
	if n > uint32(len(memstats.pause_ns)) {
		n = uint32(len(memstats.pause_ns))
	}

	// The pause buffer is circular. The most recent pause is at
	// pause_ns[(numgc-1)%len(pause_ns)], and then backward
	// from there to go back farther in time. We deliver the times
	// most recent first (in p[0]).
	p = p[:cap(p)]
	for i := uint32(0); i < n; i++ {
		j := (memstats.numgc - 1 - i) % uint32(len(memstats.pause_ns))
		p[i] = memstats.pause_ns[j]
		p[n+i] = memstats.pause_end[j]
	}

	p[n+n] = memstats.last_gc_unix
	p[n+n+1] = uint64(memstats.numgc)
	p[n+n+2] = memstats.pause_total_ns
	unlock(&mheap_.lock)
	*pauses = p[:n+n+3]
}

//go:nowritebarrier
func updatememstats() {
	memstats.mcache_inuse = uint64(mheap_.cachealloc.inuse)
	memstats.mspan_inuse = uint64(mheap_.spanalloc.inuse)
	memstats.sys = memstats.heap_sys + memstats.stacks_sys + memstats.mspan_sys +
		memstats.mcache_sys + memstats.buckhash_sys + memstats.gc_sys + memstats.other_sys

	// We also count stacks_inuse as sys memory.
	// +-? sys已经包含memstats.stacks_sys，为什么还要加memstats.stacks_inuse ？
	memstats.sys += memstats.stacks_inuse

	// Calculate memory allocator stats.
	// During program execution we only count number of frees and amount of freed memory.
	// Current number of alive object in the heap and amount of alive heap memory
	// are calculated by scanning all spans.
	// Total number of mallocs is calculated as number of frees plus number of alive objects.
	// Similarly, total amount of allocated memory is calculated as amount of freed memory
	// plus amount of alive heap memory.
	//
	// 计算内存分配器统计信息。
	// 在程序执行期间，我们只计算释放的数量和释放的内存量。
	// 通过扫描所有 span , 计算堆中当前活动对象的数量和活动堆内存的数量。
	// malloc 的对象总数是空闲对象加上活跃对象。
	// 类似地，分配的内存总量是释放的内存量加上活动堆内存量。
	memstats.alloc = 0
	memstats.total_alloc = 0
	memstats.nmalloc = 0
	memstats.nfree = 0
	for i := 0; i < len(memstats.by_size); i++ {
		memstats.by_size[i].nmalloc = 0
		memstats.by_size[i].nfree = 0
	}

	// Flush MCache's to MCentral.
	systemstack(flushallmcaches)

	// Aggregate local stats.
	cachestats()

	// Collect allocation stats. This is safe and consistent
	// because the world is stopped.
	var smallFree, totalAlloc, totalFree uint64
	// Collect per-spanclass stats.
	for spc := range mheap_.central {
		// The mcaches are now empty, so mcentral stats are
		// up-to-date.
		c := &mheap_.central[spc].mcentral
		memstats.nmalloc += c.nmalloc
		i := spanClass(spc).sizeclass()
		memstats.by_size[i].nmalloc += c.nmalloc
		totalAlloc += c.nmalloc * uint64(class_to_size[i])
	}
	// Collect per-sizeclass stats.
	for i := 0; i < _NumSizeClasses; i++ {
		if i == 0 {
			memstats.nmalloc += mheap_.nlargealloc
			totalAlloc += mheap_.largealloc
			totalFree += mheap_.largefree
			memstats.nfree += mheap_.nlargefree
			continue
		}

		// The mcache stats have been flushed to mheap_.
		memstats.nfree += mheap_.nsmallfree[i]
		memstats.by_size[i].nfree = mheap_.nsmallfree[i]
		smallFree += mheap_.nsmallfree[i] * uint64(class_to_size[i])
	}
	totalFree += smallFree

	memstats.nfree += memstats.tinyallocs
	memstats.nmalloc += memstats.tinyallocs

	// Calculate derived stats.
	memstats.total_alloc = totalAlloc
	memstats.alloc = totalAlloc - totalFree
	memstats.heap_alloc = memstats.alloc
	memstats.heap_objects = memstats.nmalloc - memstats.nfree
}

//go:nowritebarrier
func cachestats() {
	for _, p := range &allp {
		if p == nil {
			break
		}
		c := p.mcache
		if c == nil {
			continue
		}
		purgecachedstats(c)
	}
}

// flushmcache flushes the mcache of allp[i].
//
// The world must be stopped.
//
//go:nowritebarrier
func flushmcache(i int) {
	p := allp[i]
	if p == nil {
		return
	}
	c := p.mcache
	if c == nil {
		return
	}
	c.releaseAll()
	stackcache_clear(c)
}

// flushallmcaches flushes the mcaches of all Ps.
//
// The world must be stopped.
//
//go:nowritebarrier
func flushallmcaches() {
	for i := 0; i < int(gomaxprocs); i++ {
		flushmcache(i)
	}
}

//go:nosplit
func purgecachedstats(c *mcache) {
	// Protected by either heap or GC lock.
	h := &mheap_
	memstats.heap_scan += uint64(c.local_scan)
	c.local_scan = 0
	memstats.tinyallocs += uint64(c.local_tinyallocs)
	c.local_tinyallocs = 0
	memstats.nlookup += uint64(c.local_nlookup)
	c.local_nlookup = 0
	h.largefree += uint64(c.local_largefree)
	c.local_largefree = 0
	h.nlargefree += uint64(c.local_nlargefree)
	c.local_nlargefree = 0
	for i := 0; i < len(c.local_nsmallfree); i++ {
		h.nsmallfree[i] += uint64(c.local_nsmallfree[i])
		c.local_nsmallfree[i] = 0
	}
}

// Atomically increases a given *system* memory stat. We are counting on this
// stat never overflowing a uintptr, so this function must only be used for
// system memory stats.
//
// The current implementation for little endian architectures is based on
// xadduintptr(), which is less than ideal: xadd64() should really be used.
// Using xadduintptr() is a stop-gap solution until arm supports xadd64() that
// doesn't use locks.  (Locks are a problem as they require a valid G, which
// restricts their useability.)
//
// A side-effect of using xadduintptr() is that we need to check for
// overflow errors.
//go:nosplit
func mSysStatInc(sysStat *uint64, n uintptr) {
	if sys.BigEndian != 0 {
		atomic.Xadd64(sysStat, int64(n))
		return
	}
	if val := atomic.Xadduintptr((*uintptr)(unsafe.Pointer(sysStat)), n); val < n {
		print("runtime: stat overflow: val ", val, ", n ", n, "\n")
		exit(2)
	}
}

// Atomically decreases a given *system* memory stat. Same comments as
// mSysStatInc apply.
//go:nosplit
func mSysStatDec(sysStat *uint64, n uintptr) {
	if sys.BigEndian != 0 {
		atomic.Xadd64(sysStat, -int64(n))
		return
	}
	if val := atomic.Xadduintptr((*uintptr)(unsafe.Pointer(sysStat)), uintptr(-int64(n))); val+n < n {
		print("runtime: stat underflow: val ", val, ", n ", n, "\n")
		exit(2)
	}
}
