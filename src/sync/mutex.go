// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
// Mutex实现了排他锁，零值表示没有被锁
// 不能直接进行复制，作为参数的时候需要使用指针的方式
type Mutex struct {
	state int32  //表示锁的状态
	sema  uint32 //当前锁的信号量
}

/*
state:   |32|31|...|3|2|1|
         \________/ | | |
               |    | | |
               |    | | mutex的占用状态（1被占用，0可用）
               |    | |
               |    | mutex的当前goroutine是否被唤醒
               |    |
               |    是否是饥饿模式
               |
               当前阻塞在mutex上的goroutine数
*/

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked      = 1 << iota // mutex is locked
	mutexWoken                   // =2
	mutexStarving                // =4
	mutexWaiterShift = iota      // =3

	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	//
	// 为了保证锁的公平性，Mutex提供了两种调度模式
	// normal：等待者按照FIFO排队，但是被唤醒的等待者会与新到达的 goroutines 进行竞争
	//         由于可能有很多新到达的等待者，并且它们已经运行在一个CPU上，所以被唤醒的很有可能失败。
	//         失败后会排在队列头部。如果这种等待超过1ms，则切换到饥饿模式。
	// starvation: 在饥饿模式中，锁直接从解锁的goroutine传递给队列头部的等待者。
	//         即使它们到达的时候，锁刚好释放。他们也不会进入spin（自旋）状态。
	//         它们会被放到队列的尾部。
	// 如果一个等待者获取锁后发现如下情况，则会将锁切换回 normal 模式
	//   1. 它是队列中最后一个等待者
	//   2. 它等待的时间少于 1ms
	// normal 模式会有更高的性能，因为即使有很多的等待者，一个 goroutine 也可以多次获得锁。
	// starvation 模式则可以解决队尾长时间无法调度的问题
	starvationThresholdNs = 1e6 //饥饿模式的阀值，=1ms
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
// 如果已经锁住了，则调用的 goroutine 会阻塞，直到锁可用。
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	// 尝试加锁, CompareAndSwapInt32 保证加锁的原子性
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}

	var waitStartTime int64
	starving := false
	awoke := false
	iter := 0
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		// 判断当前状态是已经加锁并且不在 Starve 模式
		// runtime_canSpin 条件
		//   1. 次数限制，当前为4次
		//   2. 多核
		//   3. GOMAXPROCS>1 并且至少有一个其它正在运行的 P
		//   4. 本地 runq 为空
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			// 设置 mutexWoken 标志位，在 Unlock 的时候不去唤醒阻塞的goroutine
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			runtime_doSpin()
			iter++
			old = m.state
			continue
		}
		// 以下代码可能在没有锁或无法进入自旋状态的时候执行
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}
		if old&(mutexLocked|mutexStarving) != 0 {
			// 增加等待队列的计数
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		// 如果当前goroutine已经锁定，则切换到Starve模式
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				// 由于是当前goroutine增加的 Woken 标志位
				panic("sync: inconsistent mutex state")
			}
			new &^= mutexWoken
		}
		// 尝试加锁，或只是更改计数
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			if old&(mutexLocked|mutexStarving) == 0 {
				// 获取到锁
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			// 如果曾经放到等待队列，则放在队列头部。LIFO 后进先出
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}
			runtime_SemacquireMutex(&m.sema, queueLifo) // 等待型号量触发
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = m.state
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					panic("sync: inconsistent mutex state")
				}
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta)
				break
			}
			// 在Unlock的时候会设置 mutexWoken，所以这里需要将本地状态改成true
			awoke = true
			iter = 0
		} else {
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
// 重复调用会panic
// Mutex 并没有和goroutine绑定，允许在一个goroutine中加锁，在另一个goroutine解锁
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	// 由于只会调用一次 Unlock，所以这里直接用减法
	new := atomic.AddInt32(&m.state, -mutexLocked)
	// 判断是否是多次解锁
	if (new+mutexLocked)&mutexLocked == 0 {
		panic("sync: unlock of unlocked mutex")
	}
	if new&mutexStarving == 0 {
		old := new
		for { // 在这个过程中，可能有人在更改state，所以需要尝试多次
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			// 如果没有等待者，或锁已经被其goroutine锁了，则什么都不做。
			// 否则需要将锁传递给等待者
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			// 这里会设置 mutexWoken 标志
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				// 这里只是释放一个信号量，大家争抢这个信号量
				runtime_Semrelease(&m.sema, false)
				return
			}
			old = m.state
		}
	} else {
		// Starving mode: handoff mutex ownership to the next waiter.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		// runtime_Semrelease第二个参数表示是否传递给下一个
		// 在释放信号量的时候，并没有设置mutexLocked，这个有被唤醒的等待者进行设置
		// 但是由于mutexStarving被设置了，保证新来的goroutine不会获取它。
		runtime_Semrelease(&m.sema, true)
	}
}
