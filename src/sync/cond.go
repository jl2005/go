// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// Cond implements a condition variable, a rendezvous point
// for goroutines waiting for or announcing the occurrence
// of an event.
//
// Each Cond has an associated Locker L (often a *Mutex or *RWMutex),
// which must be held when changing the condition and
// when calling the Wait method.
//
// A Cond must not be copied after first use.
//
// Cond 实现了信号量，用于等待某些事件的发生。
// 每个Cond会有一个内联的Locker L，当改变条件或调用Wait方法之前必须持有。
//
// 下面链接的讨论中，想要使用channel替换Cond，从而废除Cond，但是由于
// channel无法实现二次通知，所以Cond会依然存在。
// https://github.com/golang/go/issues/21165
type Cond struct {
	noCopy noCopy

	// L is held while observing or changing the condition
	L Locker

	notify  notifyList
	checker copyChecker
}

// NewCond returns a new Cond with Locker l.
func NewCond(l Locker) *Cond {
	return &Cond{L: l}
}

// Wait atomically unlocks c.L and suspends execution
// of the calling goroutine. After later resuming execution,
// Wait locks c.L before returning. Unlike in other systems,
// Wait cannot return unless awoken by Broadcast or Signal.
//
// Because c.L is not locked when Wait first resumes, the caller
// typically cannot assume that the condition is true when
// Wait returns. Instead, the caller should Wait in a loop:
//
//    c.L.Lock()
//    for !condition() { //<--这里使用for循环，在唤醒之后重新检查状态
//        c.Wait()
//    }
//    ... make use of condition ...
//    c.L.Unlock()
//
// 通知协程可以这样写
//   L.Lock()  <-- 这里需要先锁定
//   c.Sign()
//   L.Unlock()
// ----------------------------------------------------------
//  在使用Broadcast的场景，则可以使用RWMutex替换Mutex，提高唤醒的并发度。
//
//  var n int
//  flag.IntVar(&n, "n", 2, "wait num")
//  flag.Parse()
//  var m sync.RWMutex
//  c := sync.NewCond(m.RLocker())
//  running := make(chan int, n)
//  awake := make(chan int, n)
//  exit := false
//  for i := 0; i < n; i++ {
//      go func(g int) {
//          m.RLock()
//          for !exit {
//              running <- g
//              c.Wait()
//              awake <- g
//          }
//          m.RUnlock()
//      }(i)
//  }
//  for i := 0; i < n; i++ {
//      <-running // Will deadlock unless n are running.
//  }
//  exit = true
//  m.Lock()
//  c.Broadcast()
//  m.Unlock()
//  seen := make([]bool, n)
//  start := time.Now()
//  for i := 0; i < n; i++ {
//      g := <-awake
//      if seen[g] {
//          fmt.Println("goroutine woke up twice")
//      }
//      seen[g] = true
//  }
//  dur := time.Since(start)
//  fmt.Println("brocadcast ok, use ", dur)
//
func (c *Cond) Wait() {
	c.checker.check()                     //检查是否被拷贝
	t := runtime_notifyListAdd(&c.notify) //加入等待队列
	c.L.Unlock()                          //解锁
	runtime_notifyListWait(&c.notify, t)  //等待信号
	c.L.Lock()                            //占有锁
}

// Signal wakes one goroutine waiting on c, if there is any.
//
// It is allowed but not required for the caller to hold c.L
// during the call.
func (c *Cond) Signal() {
	c.checker.check()
	runtime_notifyListNotifyOne(&c.notify)
}

// Broadcast wakes all goroutines waiting on c.
//
// It is allowed but not required for the caller to hold c.L
// during the call.
func (c *Cond) Broadcast() {
	c.checker.check()
	runtime_notifyListNotifyAll(&c.notify)
}

// copyChecker holds back pointer to itself to detect object copying.
type copyChecker uintptr

func (c *copyChecker) check() {
	// 首次checker并没有被赋值，所以需要先试用CAS进行赋值之后再进行比较。
	if uintptr(*c) != uintptr(unsafe.Pointer(c)) &&
		!atomic.CompareAndSwapUintptr((*uintptr)(c), 0, uintptr(unsafe.Pointer(c))) &&
		uintptr(*c) != uintptr(unsafe.Pointer(c)) {
		panic("sync.Cond is copied")
	}
}

// noCopy may be embedded into structs which must not be copied
// after the first use.
//
// See https://github.com/golang/go/issues/8005#issuecomment-190753527
// for details.
//
// 当结构体中包含noCopy，使用go vet可以检查对变量的拷贝行为，并进行提示
// assignment copies lock value to c2: sync.Cond contains sync.noCopy
// 相对于使用copyChecker动态检查变量拷贝，noCopy来得更加简单高效一些.
type noCopy struct{}

// Lock is a no-op used by -copylocks checker from `go vet`.
func (*noCopy) Lock() {}
