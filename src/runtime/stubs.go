// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/sys"
	"unsafe"
)

// Should be a built-in for unsafe.Pointer?
//go:nosplit
func add(p unsafe.Pointer, x uintptr) unsafe.Pointer {
	return unsafe.Pointer(uintptr(p) + x)
}

// getg returns the pointer to the current g.
// The compiler rewrites calls to this function into instructions
// that fetch the g directly (from TLS or from the dedicated register).
func getg() *g

// mcall switches from the g to the g0 stack and invokes fn(g),
// where g is the goroutine that made the call.
// mcall saves g's current PC/SP in g->sched so that it can be restored later.
// It is up to fn to arrange for that later execution, typically by recording
// g in a data structure, causing something to call ready(g) later.
// mcall returns to the original goroutine g later, when g has been rescheduled.
// fn must not return at all; typically it ends by calling schedule, to let the m
// run other goroutines.
//
// mcall can only be called from g stacks (not g0, not gsignal).
//
// This must NOT be go:noescape: if fn is a stack-allocated closure,
// fn puts g on a run queue, and g executes before fn returns, the
// closure will be invalidated while it is still executing.
func mcall(fn func(*g))

// systemstack runs fn on a system stack.
// If systemstack is called from the per-OS-thread (g0) stack, or
// if systemstack is called from the signal handling (gsignal) stack,
// systemstack calls fn directly and returns.
// Otherwise, systemstack is being called from the limited stack
// of an ordinary goroutine. In this case, systemstack switches
// to the per-OS-thread stack, calls fn, and switches back.
// It is common to use a func literal as the argument, in order
// to share inputs and outputs with the code around the call
// to system stack:
//
//	... set up y ...
//	systemstack(func() {
//		x = bigcall(y)
//	})
//	... use x ...
//
// systemstack 作为系统堆栈运行fn
// 如果是per-OS-thread（g0）调用的systemstack或是信号处理调用的systemstack
// systemstack直接调用fn并且返回。
// 否则，systemstack将作为有限内存的普通goroutine运行。此时，systemstack
// 切换到per-OS-thread 堆栈，调用fn，然后切换回来。它使用fn作为参数
// 以便共享输入和输出围绕在系统堆栈上下。
//go:noescape
func systemstack(fn func())

func badsystemstack() {
	throw("systemstack called from unexpected goroutine")
}

// memclrNoHeapPointers clears n bytes starting at ptr.
//
// Usually you should use typedmemclr. memclrNoHeapPointers should be
// used only when the caller knows that *ptr contains no heap pointers
// because either:
//
// 1. *ptr is initialized memory and its type is pointer-free.
//
// 2. *ptr is uninitialized memory (e.g., memory that's being reused
//    for a new allocation) and hence contains only "junk".
//
// in memclr_*.s
//
// memclrNoHeapPointers 清空从ptr开始的n个字节
// 我们通常使用typedmemclr。只有调用者知道*ptr中不含有
// 堆指针的时候才会调用memclrNoHeapPointers：
// 1. *ptr 是一个初始化的内存，并且它的类型是没有pointer
// 2. *ptr 是一个未初始化的内存（例如，内存被一个新的申请
//    使用），因此内容都是可以丢弃的
//
// 实现在 memclr_*.s
//go:noescape
func memclrNoHeapPointers(ptr unsafe.Pointer, n uintptr)

//go:linkname reflect_memclrNoHeapPointers reflect.memclrNoHeapPointers
func reflect_memclrNoHeapPointers(ptr unsafe.Pointer, n uintptr) {
	memclrNoHeapPointers(ptr, n)
}

// memmove copies n bytes from "from" to "to".
// in memmove_*.s
//go:noescape
func memmove(to, from unsafe.Pointer, n uintptr)

//go:linkname reflect_memmove reflect.memmove
func reflect_memmove(to, from unsafe.Pointer, n uintptr) {
	memmove(to, from, n)
}

// exported value for testing
var hashLoad = loadFactor

//go:nosplit
func fastrand() uint32 {
	mp := getg().m
	fr := mp.fastrand
	mx := uint32(int32(fr)>>31) & 0xa8888eef
	fr = fr<<1 ^ mx
	mp.fastrand = fr
	return fr
}

//go:nosplit
func fastrandn(n uint32) uint32 {
	// Don't be clever.
	// fastrand is not good enough for cleverness.
	// Just use mod.
	// See golang.org/issue/21806.
	return fastrand() % n
}

//go:linkname sync_fastrand sync.fastrand
func sync_fastrand() uint32 { return fastrand() }

// in asm_*.s
//go:noescape
func memequal(a, b unsafe.Pointer, size uintptr) bool

// noescape hides a pointer from escape analysis.  noescape is
// the identity function but escape analysis doesn't think the
// output depends on the input.  noescape is inlined and currently
// compiles down to zero instructions.
// USE CAREFULLY!
//go:nosplit
func noescape(p unsafe.Pointer) unsafe.Pointer {
	x := uintptr(p)
	return unsafe.Pointer(x ^ 0)
}

func cgocallback(fn, frame unsafe.Pointer, framesize, ctxt uintptr)
func gogo(buf *gobuf)
func gosave(buf *gobuf)
func mincore(addr unsafe.Pointer, n uintptr, dst *byte) int32

//go:noescape
func jmpdefer(fv *funcval, argp uintptr)
func exit1(code int32)
func asminit()
func setg(gg *g)
func breakpoint()

// reflectcall calls fn with a copy of the n argument bytes pointed at by arg.
// After fn returns, reflectcall copies n-retoffset result bytes
// back into arg+retoffset before returning. If copying result bytes back,
// the caller should pass the argument frame type as argtype, so that
// call can execute appropriate write barriers during the copy.
// Package reflect passes a frame type. In package runtime, there is only
// one call that copies results back, in cgocallbackg1, and it does NOT pass a
// frame type, meaning there are no write barriers invoked. See that call
// site for justification.
func reflectcall(argtype *_type, fn, arg unsafe.Pointer, argsize uint32, retoffset uint32)

func procyield(cycles uint32)

type neverCallThisFunction struct{}

// goexit is the return stub at the top of every goroutine call stack.
// Each goroutine stack is constructed as if goexit called the
// goroutine's entry point function, so that when the entry point
// function returns, it will return to goexit, which will call goexit1
// to perform the actual exit.
//
// This function must never be called directly. Call goexit1 instead.
// gentraceback assumes that goexit terminates the stack. A direct
// call on the stack will cause gentraceback to stop walking the stack
// prematurely and if there is leftover state it may panic.
func goexit(neverCallThisFunction)

// Not all cgocallback_gofunc frames are actually cgocallback_gofunc,
// so not all have these arguments. Mark them uintptr so that the GC
// does not misinterpret memory when the arguments are not present.
// cgocallback_gofunc is not called from go, only from cgocallback,
// so the arguments will be found via cgocallback's pointer-declared arguments.
// See the assembly implementations for more details.
func cgocallback_gofunc(fv uintptr, frame uintptr, framesize, ctxt uintptr)

// publicationBarrier performs a store/store barrier (a "publication"
// or "export" barrier). Some form of synchronization is required
// between initializing an object and making that object accessible to
// another processor. Without synchronization, the initialization
// writes and the "publication" write may be reordered, allowing the
// other processor to follow the pointer and observe an uninitialized
// object. In general, higher-level synchronization should be used,
// such as locking or an atomic pointer write. publicationBarrier is
// for when those aren't an option, such as in the implementation of
// the memory manager.
//
// There's no corresponding barrier for the read side because the read
// side naturally has a data dependency order. All architectures that
// Go supports or seems likely to ever support automatically enforce
// data dependency ordering.
func publicationBarrier()

// getcallerpc returns the program counter (PC) of its caller's caller.
// getcallersp returns the stack pointer (SP) of its caller's caller.
// For both, the argp must be a pointer to the caller's first function argument.
// The implementation may or may not use argp, depending on
// the architecture.
//
// For example:
//
//	func f(arg1, arg2, arg3 int) {
//		pc := getcallerpc(unsafe.Pointer(&arg1))
//		sp := getcallersp(unsafe.Pointer(&arg1))
//	}
//
// These two lines find the PC and SP immediately following
// the call to f (where f will return).
//
// The call to getcallerpc and getcallersp must be done in the
// frame being asked about. It would not be correct for f to pass &arg1
// to another function g and let g call getcallerpc/getcallersp.
// The call inside g might return information about g's caller or
// information about f's caller or complete garbage.
//
// The result of getcallersp is correct at the time of the return,
// but it may be invalidated by any subsequent call to a function
// that might relocate the stack in order to grow or shrink it.
// A general rule is that the result of getcallersp should be used
// immediately and can only be passed to nosplit functions.

//go:noescape
func getcallerpc(argp unsafe.Pointer) uintptr

//go:nosplit
func getcallersp(argp unsafe.Pointer) uintptr {
	return uintptr(argp) - sys.MinFrameSize
}

//go:noescape
func asmcgocall(fn, arg unsafe.Pointer) int32

// argp used in Defer structs when there is no argp.
const _NoArgs = ^uintptr(0)

func morestack()
func morestack_noctxt()
func rt0_go()

// return0 is a stub used to return 0 from deferproc.
// It is called at the very end of deferproc to signal
// the calling Go function that it should not jump
// to deferreturn.
// in asm_*.s
func return0()

// in asm_*.s
// not called directly; definitions here supply type information for traceback.
func call32(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call64(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call128(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call256(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call512(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call1024(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call2048(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call4096(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call8192(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call16384(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call32768(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call65536(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call131072(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call262144(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call524288(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call1048576(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call2097152(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call4194304(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call8388608(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call16777216(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call33554432(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call67108864(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call134217728(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call268435456(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call536870912(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call1073741824(typ, fn, arg unsafe.Pointer, n, retoffset uint32)

func systemstack_switch()

func prefetcht0(addr uintptr)
func prefetcht1(addr uintptr)
func prefetcht2(addr uintptr)
func prefetchnta(addr uintptr)

// round n up to a multiple of a.  a must be a power of 2.
func round(n, a uintptr) uintptr {
	return (n + a - 1) &^ (a - 1)
}

// checkASM returns whether assembly runtime checks have passed.
func checkASM() bool

func memequal_varlen(a, b unsafe.Pointer) bool
func eqstring(s1, s2 string) bool

// bool2int returns 0 if x is false or 1 if x is true.
func bool2int(x bool) int {
	// Avoid branches. In the SSA compiler, this compiles to
	// exactly what you would want it to.
	return int(uint8(*(*uint8)(unsafe.Pointer(&x))))
}
