// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
)

// Once is an object that will perform exactly one action.
type Once struct {
	m    Mutex
	done uint32
}

// Do calls the function f if and only if Do is being called for the
// first time for this instance of Once. In other words, given
// 	var once Once
// if once.Do(f) is called multiple times, only the first call will invoke f,
// even if f has a different value in each invocation. A new instance of
// Once is required for each function to execute.
//
// Do is intended for initialization that must be run exactly once. Since f
// is niladic, it may be necessary to use a function literal to capture the
// arguments to a function to be invoked by Do:
// 	config.once.Do(func() { config.init(filename) })
//
// Because no call to Do returns until the one call to f returns, if f causes
// Do to be called, it will deadlock.
//
// If f panics, Do considers it to have returned; future calls of Do return
// without calling f.
//
// ------------------------------------------------------------------------------
//
// 对于一个Once实例，Do 只会在第一次调用的时候执行 f 。也就是说给定一个
//   var once Once
// 如果 once.Do(f) 被调用多次，只有第一次会调用 f ，即使在每次调用中 f 有不同的值.
//
// Do 预期是执行那些只会执行一次的初始化。由于 f 并没有参数，对于需要参数的初始化，
// 则需要在外层封装一下。
//   config.once.Do(func() { config.init(filename) })
//
// 因为只有在 f 返回之后 Do 才能返回，如果 f 中又调用 Do 方法，则会导致死锁。
//
// 如果 f panic，Do 会认为它已经返回，以后对 Do 的调用会直接返回，并不会调用 f 。
//
func (o *Once) Do(f func()) {
	if atomic.LoadUint32(&o.done) == 1 {
		return
	}
	// Slow-path.
	// 其它的并发执行函数都会等在这里，从而保证只有执行过一次才会返回
	o.m.Lock()
	defer o.m.Unlock()
	if o.done == 0 {
		// 保证执行过一次之后可以标记完成，即使出现panic
		defer atomic.StoreUint32(&o.done, 1)
		f()
	}
}
