// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Memory allocator.
//
// This was originally based on tcmalloc, but has diverged quite a bit.
// http://goog-perftools.sourceforge.net/doc/tcmalloc.html

// The main allocator works in runs of pages.
// Small allocation sizes (up to and including 32 kB) are
// rounded to one of about 70 size classes, each of which
// has its own free set of objects of exactly that size.
// Any free page of memory can be split into a set of objects
// of one size class, which are then managed using a free bitmap.
//
// 主分配器是基于pages进行工作的。小对象（小于等于32KB）被划分到
// 将近70个的分组中，每一个分组都是相同大小的一组对象组成。
// 任何的内存空闲页都可以切分到一个分类中。并使用bitmap进行管理。
//
// The allocator's data structures are:
//
//	fixalloc: a free-list allocator for fixed-size off-heap objects,
//		used to manage storage used by the allocator.
//	mheap: the malloc heap, managed at page (8192-byte) granularity.
//	mspan: a run of pages managed by the mheap.
//	mcentral: collects all spans of a given size class.
//	mcache: a per-P cache of mspans with free space.
//	mstats: allocation statistics.
//
// 分配器的数据结构包括：
//
// fixalloc: 用于固定大小的堆外（off-heap）对象的自由列表分配器，
//       用于管理分配器使用的存储。
// mheap: malloc 堆，以页面（8192bytes）粒度管理。
// mspan: 由mheap管理的一系列页面
// mcntral: 收集给定大小分类的所有spans
// mcache: 具有可用空间的mspans的per-P的缓存
// mstats: 分配统计
//
// Allocating a small object proceeds up a hierarchy of caches:
//
//	1. Round the size up to one of the small size classes
//	   and look in the corresponding mspan in this P's mcache.
//	   Scan the mspan's free bitmap to find a free slot.
//	   If there is a free slot, allocate it.
//	   This can all be done without acquiring a lock.
//
//	2. If the mspan has no free slots, obtain a new mspan
//	   from the mcentral's list of mspans of the required size
//	   class that have free space.
//	   Obtaining a whole span amortizes the cost of locking
//	   the mcentral.
//
//	3. If the mcentral's mspan list is empty, obtain a run
//	   of pages from the mheap to use for the mspan.
//
//	4. If the mheap is empty or has no page runs large enough,
//	   allocate a new group of pages (at least 1MB) from the
//	   operating system. Allocating a large run of pages
//	   amortizes the cost of talking to the operating system.
//
// 分配小对象的处理方式：
//
// 1. 将size向上取整到一个分类中，然后在P的mcache中查找对应的mspan，
//    查找mspan的空闲bitmap，确定是否有空闲slot。如果有空闲slot则分配。
//    这些都可以在不获取锁的情况下完成。
//
// 2. 如果mspan没有空闲slots，从mcentral的对应分类的mspan列表中获取
//    一个新的mspan。获取整个span可以分摊对mcentral的锁开销。
//
// 3. 如果mcentral的mspan为空，则从mheap获取一系列pages，作为mspan
//
// 4. 如果mheap为空或者没有足够的pages，则从操作系统申请一组新的pages
//    （至少1MB）。申请大量页面可以分摊和操作系统通信的成本。
//
// Sweeping an mspan and freeing objects on it proceeds up a similar
// hierarchy:
//
//	1. If the mspan is being swept in response to allocation, it
//	   is returned to the mcache to satisfy the allocation.
//
//	2. Otherwise, if the mspan still has allocated objects in it,
//	   it is placed on the mcentral free list for the mspan's size
//	   class.
//
//	3. Otherwise, if all objects in the mspan are free, the mspan
//	   is now "idle", so it is returned to the mheap and no longer
//	   has a size class.
//	   This may coalesce it with adjacent idle mspans.
//
//	4. If an mspan remains idle for long enough, return its pages
//	   to the operating system.
//
// 扫描mspan并释放上面的对象执行类似的层次结构:
//
// 1. 如果对mspan的扫描是为了响应分配，则归还mcache可以满足分配。
//
// 2. 否则，如果mspan上仍然有分配的对象，它将放在mcentral的mspan
//    对应大小的空闲列表中。
//
// 3. 否则，如果mspan中的对象都释放了，mspan现在是空闲的，它将归还给
//    mheap，并且没有大小分类。它可能会与邻近的空闲mspan进行合并。
//
// 4. 如果mspan长时间空闲，将pages归还给操作系统。
//
// Allocating and freeing a large object uses the mheap
// directly, bypassing the mcache and mcentral.
//
// Free object slots in an mspan are zeroed only if mspan.needzero is
// false. If needzero is true, objects are zeroed as they are
// allocated. There are various benefits to delaying zeroing this way:
//
//	1. Stack frame allocation can avoid zeroing altogether.
//
//	2. It exhibits better temporal locality, since the program is
//	   probably about to write to the memory.
//
//	3. We don't zero pages that never get reused.
//
// 分配和释放大对象直接使用mheap，绕过mcache和mcentral。
//
// 只有mspan.needzero为flase的时候，释放对象slot时才对mspan归零。
// 如果neezero为true，对象是在分配的时候置零的。以这种方式延迟归零
// 可以有很多好处：
//
// 1. 堆栈帧分配可以完全避免归零。
//
// 2. 它可以表现更好的时间局部性，因为程序可以更好的写入内存。
//
// 3. 我们不需要对那些没有重用的页面置零。

package runtime

import (
	"runtime/internal/sys"
	"unsafe"
)

const (
	debugMalloc = false

	maxTinySize   = _TinySize
	tinySizeClass = _TinySizeClass
	maxSmallSize  = _MaxSmallSize

	pageShift = _PageShift
	pageSize  = _PageSize
	pageMask  = _PageMask
	// By construction, single page spans of the smallest object class
	// have the most objects per span.
	maxObjsPerSpan = pageSize / 8

	mSpanInUse = _MSpanInUse

	concurrentSweep = _ConcurrentSweep

	_PageSize = 1 << _PageShift
	_PageMask = _PageSize - 1

	// _64bit = 1 on 64-bit systems, 0 on 32-bit systems
	_64bit = 1 << (^uintptr(0) >> 63) / 2

	// Tiny allocator parameters, see "Tiny allocator" comment in malloc.go.
	_TinySize      = 16
	_TinySizeClass = int8(2)

	_FixAllocChunk  = 16 << 10               // Chunk size for FixAlloc
	_MaxMHeapList   = 1 << (20 - _PageShift) // Maximum page length for fixed-size list in MHeap.
	_HeapAllocChunk = 1 << 20                // Chunk size for heap growth

	// Per-P, per order stack segment cache size.
	_StackCacheSize = 32 * 1024

	// Number of orders that get caching. Order 0 is FixedStack
	// and each successive order is twice as large.
	// We want to cache 2KB, 4KB, 8KB, and 16KB stacks. Larger stacks
	// will be allocated directly.
	// Since FixedStack is different on different systems, we
	// must vary NumStackOrders to keep the same maximum cached size.
	//   OS               | FixedStack | NumStackOrders
	//   -----------------+------------+---------------
	//   linux/darwin/bsd | 2KB        | 4
	//   windows/32       | 4KB        | 3
	//   windows/64       | 8KB        | 2
	//   plan9            | 4KB        | 3
	_NumStackOrders = 4 - sys.PtrSize/4*sys.GoosWindows - 1*sys.GoosPlan9

	// Number of bits in page to span calculations (4k pages).
	// On Windows 64-bit we limit the arena to 32GB or 35 bits.
	// Windows counts memory used by page table into committed memory
	// of the process, so we can't reserve too much memory.
	// See https://golang.org/issue/5402 and https://golang.org/issue/5236.
	// On other 64-bit platforms, we limit the arena to 512GB, or 39 bits.
	// On 32-bit, we don't bother limiting anything, so we use the full 32-bit address.
	// The only exception is mips32 which only has access to low 2GB of virtual memory.
	// On Darwin/arm64, we cannot reserve more than ~5GB of virtual memory,
	// but as most devices have less than 4GB of physical memory anyway, we
	// try to be conservative here, and only ask for a 2GB heap.
	_MHeapMap_TotalBits = (_64bit*sys.GoosWindows)*35 + (_64bit*(1-sys.GoosWindows)*(1-sys.GoosDarwin*sys.GoarchArm64))*39 + sys.GoosDarwin*sys.GoarchArm64*31 + (1-_64bit)*(32-(sys.GoarchMips+sys.GoarchMipsle))
	_MHeapMap_Bits      = _MHeapMap_TotalBits - _PageShift

	// _MaxMem is the maximum heap arena size minus 1.
	//
	// On 32-bit, this is also the maximum heap pointer value,
	// since the arena starts at address 0.
	_MaxMem = 1<<_MHeapMap_TotalBits - 1

	// Max number of threads to run garbage collection.
	// 2, 3, and 4 are all plausible maximums depending
	// on the hardware details of the machine. The garbage
	// collector scales well to 32 cpus.
	_MaxGcproc = 32

	// minLegalPointer is the smallest possible legal pointer.
	// This is the smallest possible architectural page size,
	// since we assume that the first page is never mapped.
	//
	// This should agree with minZeroPage in the compiler.
	minLegalPointer uintptr = 4096
)

// physPageSize is the size in bytes of the OS's physical pages.
// Mapping and unmapping operations must be done at multiples of
// physPageSize.
//
// This must be set by the OS init code (typically in osinit) before
// mallocinit.
var physPageSize uintptr

// OS-defined helpers:
//
// sysAlloc obtains a large chunk of zeroed memory from the
// operating system, typically on the order of a hundred kilobytes
// or a megabyte.
// NOTE: sysAlloc returns OS-aligned memory, but the heap allocator
// may use larger alignment, so the caller must be careful to realign the
// memory obtained by sysAlloc.
//
// SysUnused notifies the operating system that the contents
// of the memory region are no longer needed and can be reused
// for other purposes.
// SysUsed notifies the operating system that the contents
// of the memory region are needed again.
//
// SysFree returns it unconditionally; this is only used if
// an out-of-memory error has been detected midway through
// an allocation. It is okay if SysFree is a no-op.
//
// SysReserve reserves address space without allocating memory.
// If the pointer passed to it is non-nil, the caller wants the
// reservation there, but SysReserve can still choose another
// location if that one is unavailable. On some systems and in some
// cases SysReserve will simply check that the address space is
// available and not actually reserve it. If SysReserve returns
// non-nil, it sets *reserved to true if the address space is
// reserved, false if it has merely been checked.
// NOTE: SysReserve returns OS-aligned memory, but the heap allocator
// may use larger alignment, so the caller must be careful to realign the
// memory obtained by sysAlloc.
//
// SysMap maps previously reserved address space for use.
// The reserved argument is true if the address space was really
// reserved, not merely checked.
//
// SysFault marks a (already sysAlloc'd) region to fault
// if accessed. Used only for debugging the runtime.

// mallocinit 初始化内存分配。主要执行如下步骤：
//
// 1. 条件检查
// 2. 计算各个区域的大小
// 3. 申请虚拟地址。不同的平台有不同的实现sysReserve
// 4. mheap初始化
// 5. 第一个P的 mcache 初始化
//
// 备注：其它P的mcache是在schedinit --> procresize 中初始化的
func mallocinit() {
	if class_to_size[_TinySizeClass] != _TinySize {
		throw("bad TinySizeClass")
	}

	testdefersizes()

	// Copy class sizes out for statistics table.
	for i := range class_to_size {
		memstats.by_size[i].size = uint32(class_to_size[i])
	}

	// Check physPageSize.
	if physPageSize == 0 {
		// The OS init code failed to fetch the physical page size.
		throw("failed to get system page size")
	}
	if physPageSize < minPhysPageSize {
		print("system page size (", physPageSize, ") is smaller than minimum page size (", minPhysPageSize, ")\n")
		throw("bad system page size")
	}
	if physPageSize&(physPageSize-1) != 0 {
		print("system page size (", physPageSize, ") must be a power of 2\n")
		throw("bad system page size")
	}

	// The auxiliary regions start at p and are laid out in the
	// following order: spans, bitmap, arena.
	// 辅助区域从p开始，并以如下顺序排列：spans,bitmap,arena(舞台)
	var p, pSize uintptr
	var reserved bool

	// The spans array holds one *mspan per _PageSize of arena.
	// spans数组元素是指向arena 的 _PageSize 的mspan指针
	var spansSize uintptr = (_MaxMem + 1) / _PageSize * sys.PtrSize
	spansSize = round(spansSize, _PageSize)
	// The bitmap holds 2 bits per word of arena.
	// bitmap 使用2位表示arena的每个'字'
	var bitmapSize uintptr = (_MaxMem + 1) / (sys.PtrSize * 8 / 2)
	bitmapSize = round(bitmapSize, _PageSize)

	// Set up the allocation arena, a contiguous area of memory where
	// allocated data will be found.
	// 创建一个分配arena，这是一个连续的区域，可以在其中找到分配的数据
	if sys.PtrSize == 8 {
		// On a 64-bit machine, allocate from a single contiguous reservation.
		// 512 GB (MaxMem) should be big enough for now.
		//
		// 在 64位机器上，分配是基于一个连续的预留区，当前512G（MaxMem）应该足够了。
		//
		// The code will work with the reservation at any address, but ask
		// SysReserve to use 0x0000XXc000000000 if possible (XX=00...7f).
		// Allocating a 512 GB region takes away 39 bits, and the amd64
		// doesn't let us choose the top 17 bits, so that leaves the 9 bits
		// in the middle of 0x00c0 for us to choose. Choosing 0x00c0 means
		// that the valid memory addresses will begin 0x00c0, 0x00c1, ..., 0x00df.
		// In little-endian, that's c0 00, c1 00, ..., df 00. None of those are valid
		// UTF-8 sequences, and they are otherwise as far away from
		// ff (likely a common byte) as possible. If that fails, we try other 0xXXc0
		// addresses. An earlier attempt to use 0x11f8 caused out of memory errors
		// on OS X during thread allocations.  0x00c0 causes conflicts with
		// AddressSanitizer which reserves all memory up to 0x0100.
		// These choices are both for debuggability and to reduce the
		// odds of a conservative garbage collector (as is still used in gccgo)
		// not collecting memory because some non-pointer block of memory
		// had a bit pattern that matched a memory address.
		//
		// 代码可以在任何预留区的地址都可以工作，如果可以SysReserve将会使用
		// 0x0000XXc000000000 (XX=00...7f)。分配512G的区域需要占用39位，但是
		// amd64不允许我们选择前17位，因此保留0x00c0中间的9位供我们选择。选择
		// 0x00c0 意味着有效的内存地址将是这样的0x00c0, 0x00c1, ..., 0x00df。
		// 在小头端存储，它将是c0 00, c1 00, ..., df 00。它们没有一个是有效的
		// UTF-8序列，并且尽可能远离 ff（可能是公共字节）。如果失败，我们
		// 尝试0xXXc0的地址。最初尝试使用0x11f8，但是在OS X中导致内存溢出。
		// 0x00c0与AddressSanitizer冲突，后者保留到0x0100的内存。这些选择既可以调试，
		// 也可以降低保守垃圾收集器（因为gccgo中仍然使用）不收集内存的几率，
		// 因为一些非指针内存块具有与内存地址匹配的位模式。
		//
		// Actually we reserve 544 GB (because the bitmap ends up being 32 GB)
		// but it hardly matters: e0 00 is not valid UTF-8 either.
		//
		// 因为位图会占用32GB，所以实际保留的为544GB，但它几乎没有关系：
		// e0 00 也不是有效的UTF-8字符
		//
		// If this fails we fall back to the 32 bit memory mechanism
		//
		// 如果失败，我们将回退到32位内存机制
		//
		// However, on arm64, we ignore all this advice above and slam the
		// allocation at 0x40 << 32 because when using 4k pages with 3-level
		// translation buffers, the user address space is limited to 39 bits
		// On darwin/arm64, the address space is even smaller.
		//
		// 但是，在arm64上，我们忽略以上所有的建议，并将分配大小调整为
		// 0x40 << 32，因为在darwin/arm64上，当使用带有3级缓冲区的4k页时，
		// 用户地址空间限制为39位，地址空间更小。
		arenaSize := round(_MaxMem, _PageSize)
		pSize = bitmapSize + spansSize + arenaSize + _PageSize
		// 尝试从不同的起始地址开始
		for i := 0; i <= 0x7f; i++ {
			switch {
			case GOARCH == "arm64" && GOOS == "darwin":
				p = uintptr(i)<<40 | uintptrMask&(0x0013<<28)
			case GOARCH == "arm64":
				p = uintptr(i)<<40 | uintptrMask&(0x0040<<32)
			default:
				p = uintptr(i)<<40 | uintptrMask&(0x00c0<<32)
			}
			// 向 OS 申请大小为 pSize 的连续的虚拟地址空间
			p = uintptr(sysReserve(unsafe.Pointer(p), pSize, &reserved))
			if p != 0 {
				break
			}
		}
	}

	if p == 0 {
		// On a 32-bit machine, we can't typically get away
		// with a giant virtual address space reservation.
		// Instead we map the memory information bitmap
		// immediately after the data segment, large enough
		// to handle the entire 4GB address space (256 MB),
		// along with a reservation for an initial arena.
		// When that gets used up, we'll start asking the kernel
		// for any memory anywhere.

		// 在32位机器上，我们通常无法获取这么大的预留虚拟地址空间
		// 代替的，我们在数据段之后立即创建一个内存信息位图，
		// 可以处理整个4GB地址的空间（256MB），然后是一个保留的
		// 初始化的 arena。当他们用完之后，再向内核申请其它内存。

		// We want to start the arena low, but if we're linked
		// against C code, it's possible global constructors
		// have called malloc and adjusted the process' brk.
		// Query the brk so we can avoid trying to map the
		// arena over it (which will cause the kernel to put
		// the arena somewhere else, likely at a high
		// address).
		//
		// 我们想让arena从低地址开始，但是如果我们与C代码链接，
		// 那么全局构造函数可能会调用malloc，并调整程序的brk。
		// 查询brk，这样我们就可以避免将arena映射到上面（这将
		// 导致arena放在其它地方，可能是一个高地址区域。
		procBrk := sbrk0()

		// If we fail to allocate, try again with a smaller arena.
		// This is necessary on Android L where we share a process
		// with ART, which reserves virtual memory aggressively.
		// In the worst case, fall back to a 0-sized initial arena,
		// in the hope that subsequent reservations will succeed.
		//
		// 如果分配失败，则使用更小的arena重试。在Android L中是必须的
		// 它和ART共享进程，它激进的保留虚拟内存。最坏情况下，
		// 退化到使用0-sized初始化arena，并且希望随后的保留可以成功。
		arenaSizes := []uintptr{
			512 << 20,
			256 << 20,
			128 << 20,
			0,
		}

		for _, arenaSize := range arenaSizes {
			// SysReserve treats the address we ask for, end, as a hint,
			// not as an absolute requirement. If we ask for the end
			// of the data segment but the operating system requires
			// a little more space before we can start allocating, it will
			// give out a slightly higher pointer. Except QEMU, which
			// is buggy, as usual: it won't adjust the pointer upward.
			// So adjust it upward a little bit ourselves: 1/4 MB to get
			// away from the running binary image and then round up
			// to a MB boundary.
			//
			// SysReserve提示我们要求的地址是一个端，而不是绝对的要求。
			// 如果我们要求数据段结尾，但是操作系统提供了比我们要求更多的内存
			// 它将返回一个略高的指针。除了QEMU，有一个bug之外：它不会向上调整
			// 指针。因此我们向上调整一点：1/4 MB以远离运行的二进制，
			// 然后向上舍入到MB的边界。
			p = round(firstmoduledata.end+(1<<18), 1<<20)
			pSize = bitmapSize + spansSize + arenaSize + _PageSize
			if p <= procBrk && procBrk < p+pSize {
				// Move the start above the brk,
				// leaving some room for future brk
				// expansion.
				p = round(procBrk+(1<<20), 1<<20)
			}
			p = uintptr(sysReserve(unsafe.Pointer(p), pSize, &reserved))
			if p != 0 {
				break
			}
		}
		if p == 0 {
			throw("runtime: cannot reserve arena virtual address space")
		}
	}

	// PageSize can be larger than OS definition of page size,
	// so SysReserve can give us a PageSize-unaligned pointer.
	// To overcome this we ask for PageSize more and round up the pointer.
	p1 := round(p, _PageSize)
	pSize -= p1 - p

	spansStart := p1
	p1 += spansSize
	mheap_.bitmap = p1 + bitmapSize
	p1 += bitmapSize
	if sys.PtrSize == 4 {
		// Set arena_start such that we can accept memory
		// reservations located anywhere in the 4GB virtual space.
		mheap_.arena_start = 0
	} else {
		mheap_.arena_start = p1
	}
	mheap_.arena_end = p + pSize
	mheap_.arena_used = p1
	mheap_.arena_alloc = p1
	mheap_.arena_reserved = reserved

	if mheap_.arena_start&(_PageSize-1) != 0 {
		println("bad pagesize", hex(p), hex(p1), hex(spansSize), hex(bitmapSize), hex(_PageSize), "start", hex(mheap_.arena_start))
		throw("misrounded allocation in mallocinit")
	}

	// Initialize the rest of the allocator.
	mheap_.init(spansStart, spansSize)
	_g_ := getg()
	_g_.m.mcache = allocmcache()
}

// sysAlloc allocates the next n bytes from the heap arena. The
// returned pointer is always _PageSize aligned and between
// h.arena_start and h.arena_end. sysAlloc returns nil on failure.
// There is no corresponding free function.
func (h *mheap) sysAlloc(n uintptr) unsafe.Pointer {
	// strandLimit is the maximum number of bytes to strand from
	// the current arena block. If we would need to strand more
	// than this, we fall back to sysAlloc'ing just enough for
	// this allocation.
	const strandLimit = 16 << 20

	if n > h.arena_end-h.arena_alloc {
		// If we haven't grown the arena to _MaxMem yet, try
		// to reserve some more address space.
		p_size := round(n+_PageSize, 256<<20)
		new_end := h.arena_end + p_size // Careful: can overflow
		if h.arena_end <= new_end && new_end-h.arena_start-1 <= _MaxMem {
			// TODO: It would be bad if part of the arena
			// is reserved and part is not.
			var reserved bool
			p := uintptr(sysReserve(unsafe.Pointer(h.arena_end), p_size, &reserved))
			if p == 0 {
				// TODO: Try smaller reservation
				// growths in case we're in a crowded
				// 32-bit address space.
				goto reservationFailed
			}
			// p can be just about anywhere in the address
			// space, including before arena_end.
			if p == h.arena_end {
				// The new block is contiguous with
				// the current block. Extend the
				// current arena block.
				h.arena_end = new_end
				h.arena_reserved = reserved
			} else if h.arena_start <= p && p+p_size-h.arena_start-1 <= _MaxMem && h.arena_end-h.arena_alloc < strandLimit {
				// We were able to reserve more memory
				// within the arena space, but it's
				// not contiguous with our previous
				// reservation. It could be before or
				// after our current arena_used.
				//
				// Keep everything page-aligned.
				// Our pages are bigger than hardware pages.
				h.arena_end = p + p_size
				p = round(p, _PageSize)
				h.arena_alloc = p
				h.arena_reserved = reserved
			} else {
				// We got a mapping, but either
				//
				// 1) It's not in the arena, so we
				// can't use it. (This should never
				// happen on 32-bit.)
				//
				// 2) We would need to discard too
				// much of our current arena block to
				// use it.
				//
				// We haven't added this allocation to
				// the stats, so subtract it from a
				// fake stat (but avoid underflow).
				//
				// We'll fall back to a small sysAlloc.
				stat := uint64(p_size)
				sysFree(unsafe.Pointer(p), p_size, &stat)
			}
		}
	}

	if n <= h.arena_end-h.arena_alloc {
		// Keep taking from our reservation.
		p := h.arena_alloc
		sysMap(unsafe.Pointer(p), n, h.arena_reserved, &memstats.heap_sys)
		h.arena_alloc += n
		if h.arena_alloc > h.arena_used {
			h.setArenaUsed(h.arena_alloc, true)
		}

		if p&(_PageSize-1) != 0 {
			throw("misrounded allocation in MHeap_SysAlloc")
		}
		return unsafe.Pointer(p)
	}

reservationFailed:
	// If using 64-bit, our reservation is all we have.
	if sys.PtrSize != 4 {
		return nil
	}

	// On 32-bit, once the reservation is gone we can
	// try to get memory at a location chosen by the OS.
	p_size := round(n, _PageSize) + _PageSize
	p := uintptr(sysAlloc(p_size, &memstats.heap_sys))
	if p == 0 {
		return nil
	}

	if p < h.arena_start || p+p_size-h.arena_start > _MaxMem {
		// This shouldn't be possible because _MaxMem is the
		// whole address space on 32-bit.
		top := uint64(h.arena_start) + _MaxMem
		print("runtime: memory allocated by OS (", hex(p), ") not in usable range [", hex(h.arena_start), ",", hex(top), ")\n")
		sysFree(unsafe.Pointer(p), p_size, &memstats.heap_sys)
		return nil
	}

	p += -p & (_PageSize - 1)
	if p+n > h.arena_used {
		h.setArenaUsed(p+n, true)
	}

	if p&(_PageSize-1) != 0 {
		throw("misrounded allocation in MHeap_SysAlloc")
	}
	return unsafe.Pointer(p)
}

// base address for all 0-byte allocations
var zerobase uintptr

// nextFreeFast returns the next free object if one is quickly available.
// Otherwise it returns 0.
// nextFreeFast 返回下一个空闲对象，否则返回0
func nextFreeFast(s *mspan) gclinkptr {
	// 基于缓存查找可用位
	theBit := sys.Ctz64(s.allocCache) // Is there a free object in the allocCache?
	// 如果有可用的
	if theBit < 64 {
		result := s.freeindex + uintptr(theBit)
		if result < s.nelems {
			freeidx := result + 1
			if freeidx%64 == 0 && freeidx != s.nelems {
				return 0
			}
			// 调整可用内存地址
			s.allocCache >>= uint(theBit + 1)
			s.freeindex = freeidx
			v := gclinkptr(result*s.elemsize + s.base())
			s.allocCount++
			return v
		}
	}
	return 0
}

// nextFree returns the next free object from the cached span if one is available.
// Otherwise it refills the cache with a span with an available object and
// returns that object along with a flag indicating that this was a heavy
// weight allocation. If it is a heavy weight allocation the caller must
// determine whether a new GC cycle needs to be started or if the GC is active
// whether this goroutine needs to assist the GC.
//
// +-? 这个翻译的有问题
// nextFree 如果存在一个缓存的空闲对象，则返回，否则使用一个可用对象的span填充
// cache，然后使用一个权重标志填充span。如果是高权重分配，调用者必须决定是否
// 需要启动一个GC，或者GC是否处于活动状态，是否需要将goroutine给GC
func (c *mcache) nextFree(spc spanClass) (v gclinkptr, s *mspan, shouldhelpgc bool) {
	s = c.alloc[spc]
	shouldhelpgc = false
	// 获取下一个可用object索引
	freeIndex := s.nextFreeIndex()
	// 没有空余则扩容
	if freeIndex == s.nelems {
		// The span is full.
		if uintptr(s.allocCount) != s.nelems {
			println("runtime: s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
			throw("s.allocCount != s.nelems && freeIndex == s.nelems")
		}
		// 从mcentral获取内存
		systemstack(func() {
			c.refill(spc)
		})
		shouldhelpgc = true
		s = c.alloc[spc]

		// 重新获取可用的索引
		freeIndex = s.nextFreeIndex()
	}

	if freeIndex >= s.nelems {
		throw("freeIndex is not valid")
	}

	v = gclinkptr(freeIndex*s.elemsize + s.base())
	s.allocCount++
	if uintptr(s.allocCount) > s.nelems {
		println("s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
		throw("s.allocCount > s.nelems")
	}
	return
}

// Allocate an object of size bytes.
// Small objects are allocated from the per-P cache's free lists.
// Large objects (> 32 kB) are allocated straight from the heap.
//
// Allocate size byte的一个对象
// 小对象直接从per-P cache的空闲列表中获取
// 大对象（>32KB）直接从heap分配
func mallocgc(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	if gcphase == _GCmarktermination {
		throw("mallocgc called with gcphase == _GCmarktermination")
	}

	if size == 0 {
		return unsafe.Pointer(&zerobase)
	}

	if debug.sbrk != 0 {
		align := uintptr(16)
		if typ != nil {
			align = uintptr(typ.align)
		}
		return persistentalloc(size, align, &memstats.other_sys)
	}

	// assistG is the G to charge for this allocation, or nil if
	// GC is not currently active.
	// assistG 是当前分配服务的G，如果GC没有激活，则为nil
	var assistG *g
	if gcBlackenEnabled != 0 {
		// Charge the current user G for this allocation.
		assistG = getg()
		if assistG.m.curg != nil {
			assistG = assistG.m.curg
		}
		// Charge the allocation against the G. We'll account
		// for internal fragmentation at the end of mallocgc.
		assistG.gcAssistBytes -= int64(size)

		if assistG.gcAssistBytes < 0 {
			// This G is in debt. Assist the GC to correct
			// this before allocating. This must happen
			// before disabling preemption.
			gcAssistAlloc(assistG)
		}
	}

	// Set mp.mallocing to keep from being preempted by GC.
	// 设置 mp.mallocing 防止GC抢占
	mp := acquirem()
	if mp.mallocing != 0 {
		throw("malloc deadlock")
	}
	if mp.gsignal == getg() {
		throw("malloc during signal")
	}
	mp.mallocing = 1

	shouldhelpgc := false
	dataSize := size
	// 获取mcache
	c := gomcache()
	var x unsafe.Pointer
	noscan := typ == nil || typ.kind&kindNoPointers != 0
	if size <= maxSmallSize {
		// 小于32k的内存分配
		if noscan && size < maxTinySize {
			// 类型内部没有指针 && 小于16byte的内存分配

			// Tiny allocator.
			//
			// Tiny allocator combines several tiny allocation requests
			// into a single memory block. The resulting memory block
			// is freed when all subobjects are unreachable. The subobjects
			// must be noscan (don't have pointers), this ensures that
			// the amount of potentially wasted memory is bounded.
			//
			// 微型分配器
			//
			// 微型分配器将几个微小的分配请求组合到一个内存块中。当所有
			// 子对象都无法访问时，将释放生成的内存块。子对象必须是
			// noscan（没有指针），这可以确保可能浪费的内存量受到限制。
			//
			// Size of the memory block used for combining (maxTinySize) is tunable.
			// Current setting is 16 bytes, which relates to 2x worst case memory
			// wastage (when all but one subobjects are unreachable).
			// 8 bytes would result in no wastage at all, but provides less
			// opportunities for combining.
			// 32 bytes provides more opportunities for combining,
			// but can lead to 4x worst case wastage.
			// The best case winning is 8x regardless of block size.
			//
			// 用于组合的内存块的大小（maxTinySize）是可调的。
			// 当前设置为16个字节，这可能造成了最大2倍的内存浪费（当除了一个
			// 子对象之外的所有子对象都无法访问时）。 8字节时将完全没有浪费，
			// 但提供较少的组合机会。32 字节提供了更多的组合机会，
			// 但可能导致4倍最坏情况下的浪费。
			// 无论块大小如何，最好的情况都是8倍。
			//
			// Objects obtained from tiny allocator must not be freed explicitly.
			// So when an object will be freed explicitly, we ensure that
			// its size >= maxTinySize.
			//
			// 从微小分配器获得的对象不能显式释放。因此，当显式释放对象时，我们确保其大小 >= maxTinySize。
			//
			// SetFinalizer has a special case for objects potentially coming
			// from tiny allocator, it such case it allows to set finalizers
			// for an inner byte of a memory block.
			//
			// SetFinalizer对于可能来自微小分配器的对象有一个特殊情况，
			// 它允许为内存块的内部字节设置终结器。
			//
			// The main targets of tiny allocator are small strings and
			// standalone escaping variables. On a json benchmark
			// the allocator reduces number of allocations by ~12% and
			// reduces heap size by ~20%.
			//
			// 微分配器的主要目标是小字符串和独立的转义变量。
			// 在json基准测试中，分配器将分配数量减少了大约12％，
			// 并将堆大小减少了大约20％。
			off := c.tinyoffset // off 表示tiny区域的偏移量
			// Align tiny pointer for required (conservative) alignment.
			// 根据size的规格，将off向上对齐
			if size&7 == 0 {
				off = round(off, 8)
			} else if size&3 == 0 {
				off = round(off, 4)
			} else if size&1 == 0 {
				off = round(off, 2)
			}
			if off+size <= maxTinySize && c.tiny != 0 {
				// 当前的tiny有空间，则直接放到这个tiny中
				// The object fits into existing tiny block.
				x = unsafe.Pointer(c.tiny + off)
				c.tinyoffset = off + size
				c.local_tinyallocs++
				mp.mallocing = 0
				releasem(mp)
				return x
			}
			// Allocate a new maxTinySize block.
			// 申请一个新的 maxTinySize 块
			span := c.alloc[tinySpanClass]
			v := nextFreeFast(span)
			if v == 0 {
				v, _, shouldhelpgc = c.nextFree(tinySpanClass)
			}
			x = unsafe.Pointer(v)
			(*[2]uint64)(x)[0] = 0
			(*[2]uint64)(x)[1] = 0
			// See if we need to replace the existing tiny block with the new one
			// based on amount of remaining free space.
			// 对比新旧两个tiny 内存，保留剩余空间大的那个
			if size < c.tinyoffset || c.tiny == 0 {
				c.tiny = uintptr(x)
				c.tinyoffset = size
			}
			size = maxTinySize
		} else {
			// 16byte -- 32KB 的内存分配
			// +- 如何将内存大小映射到对应的span ？
			// 由于不能将32k内所有的数字都指定一个映射，所以这里采用了分块的方式进行映射：
			// 1. 对于小于 1024byte 的内存，按照8 byte 的步长进行划分
			// 2. 对于1024 byte - 32k 的内存按照128 byte的步长进行划分
			// 通过这种方式可以极大的减少映射数组的大小
			var sizeclass uint8
			if size <= smallSizeMax-8 {
				// size_to_class8 按照8 byte进行划分，对应的sizeclass
				sizeclass = size_to_class8[(size+smallSizeDiv-1)/smallSizeDiv]
			} else {
				sizeclass = size_to_class128[(size-smallSizeMax+largeSizeDiv-1)/largeSizeDiv]
			}
			// 获取class对应的大小
			size = uintptr(class_to_size[sizeclass])
			// sizeclass << 1 | noscan
			spc := makeSpanClass(sizeclass, noscan)
			span := c.alloc[spc]
			v := nextFreeFast(span)
			if v == 0 {
				v, span, shouldhelpgc = c.nextFree(spc)
			}
			x = unsafe.Pointer(v)
			if needzero && span.needzero != 0 {
				memclrNoHeapPointers(unsafe.Pointer(v), size)
			}
		}
	} else {
		// 大于32KB的内存分配，直接从mheap中分配
		var s *mspan
		shouldhelpgc = true
		systemstack(func() {
			s = largeAlloc(size, needzero, noscan)
		})
		s.freeindex = 1
		s.allocCount = 1
		x = unsafe.Pointer(s.base())
		size = s.elemsize
	}

	var scanSize uintptr
	if !noscan {
		// If allocating a defer+arg block, now that we've picked a malloc size
		// large enough to hold everything, cut the "asked for" size down to
		// just the defer header, so that the GC bitmap will record the arg block
		// as containing nothing at all (as if it were unused space at the end of
		// a malloc block caused by size rounding).
		// The defer arg areas are scanned as part of scanstack.
		if typ == deferType {
			dataSize = unsafe.Sizeof(_defer{})
		}
		heapBitsSetType(uintptr(x), size, dataSize, typ)
		if dataSize > typ.size {
			// Array allocation. If there are any
			// pointers, GC has to scan to the last
			// element.
			if typ.ptrdata != 0 {
				scanSize = dataSize - typ.size + typ.ptrdata
			}
		} else {
			scanSize = typ.ptrdata
		}
		c.local_scan += scanSize
	}

	// Ensure that the stores above that initialize x to
	// type-safe memory and set the heap bits occur before
	// the caller can make x observable to the garbage
	// collector. Otherwise, on weakly ordered machines,
	// the garbage collector could follow a pointer to x,
	// but see uninitialized memory or stale heap bits.
	publicationBarrier()

	// Allocate black during GC.
	// All slots hold nil so no scanning is needed.
	// This may be racing with GC so do it atomically if there can be
	// a race marking the bit.
	if gcphase != _GCoff {
		gcmarknewobject(uintptr(x), size, scanSize)
	}

	if raceenabled {
		racemalloc(x, size)
	}

	if msanenabled {
		msanmalloc(x, size)
	}

	mp.mallocing = 0
	releasem(mp)

	if debug.allocfreetrace != 0 {
		tracealloc(x, size, typ)
	}

	if rate := MemProfileRate; rate > 0 {
		if size < uintptr(rate) && int32(size) < c.next_sample {
			c.next_sample -= int32(size)
		} else {
			mp := acquirem()
			profilealloc(mp, x, size)
			releasem(mp)
		}
	}

	if assistG != nil {
		// Account for internal fragmentation in the assist
		// debt now that we know it.
		assistG.gcAssistBytes -= int64(size - dataSize)
	}

	if shouldhelpgc {
		if t := (gcTrigger{kind: gcTriggerHeap}); t.test() {
			gcStart(gcBackgroundMode, t)
		}
	}

	return x
}

func largeAlloc(size uintptr, needzero bool, noscan bool) *mspan {
	// print("largeAlloc size=", size, "\n")

	if size+_PageSize < size {
		throw("out of memory")
	}
	npages := size >> _PageShift
	if size&_PageMask != 0 {
		npages++
	}

	// Deduct credit for this span allocation and sweep if
	// necessary. mHeap_Alloc will also sweep npages, so this only
	// pays the debt down to npage pages.
	deductSweepCredit(npages*_PageSize, npages)

	s := mheap_.alloc(npages, makeSpanClass(0, noscan), true, needzero)
	if s == nil {
		throw("out of memory")
	}
	s.limit = s.base() + size
	heapBitsForSpan(s.base()).initSpan(s)
	return s
}

// implementation of new builtin
// compiler (both frontend and SSA backend) knows the signature
// of this function
func newobject(typ *_type) unsafe.Pointer {
	return mallocgc(typ.size, typ, true)
}

//go:linkname reflect_unsafe_New reflect.unsafe_New
func reflect_unsafe_New(typ *_type) unsafe.Pointer {
	return newobject(typ)
}

// newarray allocates an array of n elements of type typ.
func newarray(typ *_type, n int) unsafe.Pointer {
	if n < 0 || uintptr(n) > maxSliceCap(typ.size) {
		panic(plainError("runtime: allocation size out of range"))
	}
	return mallocgc(typ.size*uintptr(n), typ, true)
}

//go:linkname reflect_unsafe_NewArray reflect.unsafe_NewArray
func reflect_unsafe_NewArray(typ *_type, n int) unsafe.Pointer {
	return newarray(typ, n)
}

func profilealloc(mp *m, x unsafe.Pointer, size uintptr) {
	mp.mcache.next_sample = nextSample()
	mProf_Malloc(x, size)
}

// nextSample returns the next sampling point for heap profiling.
// It produces a random variable with a geometric distribution and
// mean MemProfileRate. This is done by generating a uniformly
// distributed random number and applying the cumulative distribution
// function for an exponential.
func nextSample() int32 {
	if GOOS == "plan9" {
		// Plan 9 doesn't support floating point in note handler.
		if g := getg(); g == g.m.gsignal {
			return nextSampleNoFP()
		}
	}

	period := MemProfileRate

	// make nextSample not overflow. Maximum possible step is
	// -ln(1/(1<<kRandomBitCount)) * period, approximately 20 * period.
	switch {
	case period > 0x7000000:
		period = 0x7000000
	case period == 0:
		return 0
	}

	// Let m be the sample rate,
	// the probability distribution function is m*exp(-mx), so the CDF is
	// p = 1 - exp(-mx), so
	// q = 1 - p == exp(-mx)
	// log_e(q) = -mx
	// -log_e(q)/m = x
	// x = -log_e(q) * period
	// x = log_2(q) * (-log_e(2)) * period    ; Using log_2 for efficiency
	const randomBitCount = 26
	q := fastrand()%(1<<randomBitCount) + 1
	qlog := fastlog2(float64(q)) - randomBitCount
	if qlog > 0 {
		qlog = 0
	}
	const minusLog2 = -0.6931471805599453 // -ln(2)
	return int32(qlog*(minusLog2*float64(period))) + 1
}

// nextSampleNoFP is similar to nextSample, but uses older,
// simpler code to avoid floating point.
func nextSampleNoFP() int32 {
	// Set first allocation sample size.
	rate := MemProfileRate
	if rate > 0x3fffffff { // make 2*rate not overflow
		rate = 0x3fffffff
	}
	if rate != 0 {
		return int32(fastrand() % uint32(2*rate))
	}
	return 0
}

type persistentAlloc struct {
	base unsafe.Pointer
	off  uintptr
}

var globalAlloc struct {
	mutex
	persistentAlloc
}

// Wrapper around sysAlloc that can allocate small chunks.
// There is no associated free operation.
// Intended for things like function/type/debug-related persistent data.
// If align is 0, uses default align (currently 8).
// The returned memory will be zeroed.
//
// Consider marking persistentalloc'd types go:notinheap.
func persistentalloc(size, align uintptr, sysStat *uint64) unsafe.Pointer {
	var p unsafe.Pointer
	systemstack(func() {
		p = persistentalloc1(size, align, sysStat)
	})
	return p
}

// Must run on system stack because stack growth can (re)invoke it.
// See issue 9174.
//go:systemstack
func persistentalloc1(size, align uintptr, sysStat *uint64) unsafe.Pointer {
	const (
		chunk    = 256 << 10
		maxBlock = 64 << 10 // VM reservation granularity is 64K on windows
	)

	if size == 0 {
		throw("persistentalloc: size == 0")
	}
	if align != 0 {
		if align&(align-1) != 0 {
			throw("persistentalloc: align is not a power of 2")
		}
		if align > _PageSize {
			throw("persistentalloc: align is too large")
		}
	} else {
		align = 8
	}

	if size >= maxBlock {
		return sysAlloc(size, sysStat)
	}

	mp := acquirem()
	var persistent *persistentAlloc
	if mp != nil && mp.p != 0 {
		persistent = &mp.p.ptr().palloc
	} else {
		lock(&globalAlloc.mutex)
		persistent = &globalAlloc.persistentAlloc
	}
	persistent.off = round(persistent.off, align)
	if persistent.off+size > chunk || persistent.base == nil {
		persistent.base = sysAlloc(chunk, &memstats.other_sys)
		if persistent.base == nil {
			if persistent == &globalAlloc.persistentAlloc {
				unlock(&globalAlloc.mutex)
			}
			throw("runtime: cannot allocate memory")
		}
		persistent.off = 0
	}
	p := add(persistent.base, persistent.off)
	persistent.off += size
	releasem(mp)
	if persistent == &globalAlloc.persistentAlloc {
		unlock(&globalAlloc.mutex)
	}

	if sysStat != &memstats.other_sys {
		mSysStatInc(sysStat, size)
		mSysStatDec(&memstats.other_sys, size)
	}
	return p
}
