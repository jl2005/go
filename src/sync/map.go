// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// Map is a concurrent map with amortized-constant-time loads, stores, and deletes.
// It is safe for multiple goroutines to call a Map's methods concurrently.
//
// It is optimized for use in concurrent loops with keys that are
// stable over time, and either few steady-state stores, or stores
// localized to one goroutine per key.
//
// For use cases that do not share these attributes, it will likely have
// comparable or worse performance and worse type safety than an ordinary
// map paired with a read-write mutex.
//
// The zero Map is valid and empty.
//
// A Map must not be copied after first use.
//
// Map 是一个并发的map，并实现了amortized-constant-time的load、store和delete
// [What is amortized time?](https://mortoray.com/2014/08/11/what-is-amortized-time/)
// [Constant Amortized Time](https://stackoverflow.com/questions/200384/constant-amortized-time/38261380)
// 多个goroutine并发访问Map的方法是安全的。
//
// +-? 它优化了对key的并发循环，有很少的store，或每个key存储到goroutine的本地.
//
// +- 对于不具有这些属性的场景，与使用读写锁的map相比，它可能持平或更差的性能，更坏的类型安全。
//
// 空 Map 是有效的和空的。
//
// Map 在使用之后不允许拷贝，可以通过指针进行传递
type Map struct {
	mu Mutex

	// read contains the portion of the map's contents that are safe for
	// concurrent access (with or without mu held).
	//
	// The read field itself is always safe to load, but must only be stored with
	// mu held.
	//
	// Entries stored in read may be updated concurrently without mu, but updating
	// a previously-expunged entry requires that the entry be copied to the dirty
	// map and unexpunged with mu held.
	//
	// read 包含一个并发安全访问的map指针，不需要持有mu
	// read 字段在load的时候总是安全的，但是在stored的时候需要持有mu
	// 在read中的entry可能在没有mu的时候并发被更新，但是更新一个已经标记为expuned的
	// entry的时候，需要持有锁的时候，将entry拷贝到dirty map，并且unexpunged
	read atomic.Value // readOnly

	// dirty contains the portion of the map's contents that require mu to be
	// held. To ensure that the dirty map can be promoted to the read map quickly,
	// it also includes all of the non-expunged entries in the read map.
	//
	// Expunged entries are not stored in the dirty map. An expunged entry in the
	// clean map must be unexpunged and added to the dirty map before a new value
	// can be stored to it.
	//
	// If the dirty map is nil, the next write to the map will initialize it by
	// making a shallow copy of the clean map, omitting stale entries.
	//
	// dirty 包含map内容的一部分，需要持有mu。为了保证dirty快速的提升为read map,
	// 它也需要包含read map 中“所有” non-expunged 的entry
	//
	// expunged entry没有存储在dirty map 中。一个expunged entry在添加新key的时候，首先需要
	// 先置unexpected，并且将其加入到dirty map.
	//
	// 如果dirty map是nil，下次写map会使用浅拷贝一个干净的map的方式初始化它,跳过过期的entry
	dirty map[interface{}]*entry

	// misses counts the number of loads since the read map was last updated that
	// needed to lock mu to determine whether the key was present.
	//
	// Once enough misses have occurred to cover the cost of copying the dirty
	// map, the dirty map will be promoted to the read map (in the unamended
	// state) and the next store to the map will make a new dirty copy.
	//
	// misses 记录从上次更新之后，需要锁定mu才能确定key是否存在的次数。
	// 一旦misses次数达到拷贝dirty map的代价，dirty map将会提升为read map,
	// 下次store 的时候将会创建一个全新的dirty。
	misses int
}

// readOnly is an immutable struct stored atomically in the Map.read field.
// readOnly 是一个不可改变的存储，以原子的方式存储在Map.read字段
type readOnly struct {
	m       map[interface{}]*entry
	amended bool // true if the dirty map contains some key not in m.
}

// expunged is an arbitrary pointer that marks entries which have been deleted
// from the dirty map.
// expunged 是一个任意指针，用于标记entry已经从dirty map中删除
var expunged = unsafe.Pointer(new(interface{}))

// An entry is a slot in the map corresponding to a particular key.
// entry map中特定key对应的slot
type entry struct {
	// p points to the interface{} value stored for the entry.
	//
	// If p == nil, the entry has been deleted and m.dirty == nil.
	//
	// If p == expunged, the entry has been deleted, m.dirty != nil, and the entry
	// is missing from m.dirty.
	//
	// Otherwise, the entry is valid and recorded in m.read.m[key] and, if m.dirty
	// != nil, in m.dirty[key].
	//
	// An entry can be deleted by atomic replacement with nil: when m.dirty is
	// next created, it will atomically replace nil with expunged and leave
	// m.dirty[key] unset.
	//
	// An entry's associated value can be updated by atomic replacement, provided
	// p != expunged. If p == expunged, an entry's associated value can be updated
	// only after first setting m.dirty[key] = e so that lookups using the dirty
	// map find the entry.
	//
	// p 指向entry存储的值
	//
	// p == nil，entry已经被删除，并且m.dirty==nil
	// p == expunged，entry已经被删除，m.dirty!=nil，entry不在m.dirty中。
	//           这个是在tryExpungeLocked 中修改的。
	// 其它，entry是正常的存储在m.read.m[key]中，如果m.dirty!=nil也存在于m.dirty[key]
	//
	// 在删除的时候，entry可以原子的替代为nil：当m.dirty下次创建的时候，他将原子使用expunged
	// 替代为nil，并且不会设置m.dirty[key]。
	//
	// 如果p!=expunged，entry关联的value可以被原子的替代。如果 p==expunged，entry只有在
	// m.dirty[key] = e 之后才可以被更新，以便可以在dirty map中找到entry
	p unsafe.Pointer // *interface{}
}

// p 的状态转换
//     tryExpungeLocked: 在拷贝m.read 到m.dirty时，已经删除的数据设置为expunged
// nil -----------------------------------------------------------------------> expunged
//
//           unexpungeLocked: 重新设置已经删除的数据
// expunged -----------------------------------------> nil

func newEntry(i interface{}) *entry {
	return &entry{p: unsafe.Pointer(&i)}
}

// Load returns the value stored in the map for a key, or nil if no
// value is present.
// The ok result indicates whether value was found in the map.
//
// Load 返回key对应的value，如果不存在则返回nil
// ok 指示map中是否存在对应的value
func (m *Map) Load(key interface{}) (value interface{}, ok bool) {
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]
	// +- 这里并没有直接判断为ok==true，而是将判断放到最后，代码要明确很多
	if !ok && read.amended {
		m.mu.Lock()
		// Avoid reporting a spurious miss if m.dirty got promoted while we were
		// blocked on m.mu. (If further loads of the same key will not miss, it's
		// not worth copying the dirty map for this key.)
		//
		// 防止当我们阻塞在m.mu的时候，m.dirty升级了， 而汇报一个假的miss。
		// 如果将来同样的key没有miss，它将不值得拷贝dirty map。
		read, _ = m.read.Load().(readOnly)
		e, ok = read.m[key]
		if !ok && read.amended { // 有尝试了一次
			e, ok = m.dirty[key]
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			// 不管entry是否存在，都记录miss：这个key通过slow path获取
			// 直到dirty map提升为read map
			m.missLocked()
		}
		m.mu.Unlock()
	}
	if !ok {
		return nil, false
	}
	return e.load()
}

func (e *entry) load() (value interface{}, ok bool) {
	p := atomic.LoadPointer(&e.p)
	if p == nil || p == expunged {
		return nil, false
	}
	return *(*interface{})(p), true
}

// Store sets the value for a key.
// Store 设置key的value
func (m *Map) Store(key, value interface{}) {
	read, _ := m.read.Load().(readOnly)
	// 由于m.read和m.dirty共用 entry，所以直接修改就可以
	if e, ok := read.m[key]; ok && e.tryStore(&value) {
		return
	}

	m.mu.Lock()
	read, _ = m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok { // 如果可以在read中找到entry
		if e.unexpungeLocked() {
			// The entry was previously expunged, which implies that there is a
			// non-nil dirty map and this entry is not in it.
			//
			// entry之前是 expunged, 也就是意味着dirty非空，并且entry不在其中
			m.dirty[key] = e
		}
		e.storeLocked(&value)
	} else if e, ok := m.dirty[key]; ok { // 如果可以在dirtry中找到entry
		e.storeLocked(&value)
	} else { // 新加入
		if !read.amended {
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			//
			// 第一次添加新key到dirty map，标记amended=true表示read是不完整的
			m.dirtyLocked() // 将read中的未删除的数据都拷贝到dirty map中
			m.read.Store(readOnly{m: read.m, amended: true})
		}
		m.dirty[key] = newEntry(value)
	}
	m.mu.Unlock()
}

// tryStore stores a value if the entry has not been expunged.
//
// If the entry is expunged, tryStore returns false and leaves the entry
// unchanged.
//
// tryStore 在entry没有被抹除的时候存储value
//
// 如果entry已经被抹除，tryStore返回false，并且不会更改entry
func (e *entry) tryStore(i *interface{}) bool {
	p := atomic.LoadPointer(&e.p)
	if p == expunged {
		return false
	}
	for {
		if atomic.CompareAndSwapPointer(&e.p, p, unsafe.Pointer(i)) {
			return true
		}
		p = atomic.LoadPointer(&e.p)
		if p == expunged {
			return false
		}
	}

	// +- 如下写法应该更加简练
	/*
		var p pointer
		for {
			p = atomic.LoadPointer(&e.p)
			if p == expunged {
				return false
			}
			if atomic.CompareAndSwapPointer(&e.p, p, unsafe.Pointer(i)) {
				return true
			}
		}
	*/
}

// unexpungeLocked ensures that the entry is not marked as expunged.
//
// If the entry was previously expunged, it must be added to the dirty map
// before m.mu is unlocked.
//
// unexpungeLocked 保证entry没有被标记为expunged
// 如果entry已经是expunged，它必须在m.mu 解锁之前添加到dirty map
func (e *entry) unexpungeLocked() (wasExpunged bool) {
	return atomic.CompareAndSwapPointer(&e.p, expunged, nil)
}

// storeLocked unconditionally stores a value to the entry.
//
// The entry must be known not to be expunged.
//
// storeLocked 无条件的存储value到entry
// entry 必须不能是 expunged
func (e *entry) storeLocked(i *interface{}) {
	atomic.StorePointer(&e.p, unsafe.Pointer(i))
}

// LoadOrStore returns the existing value for the key if present.
// Otherwise, it stores and returns the given value.
// The loaded result is true if the value was loaded, false if stored.
//
// LoadOrStore 有数据就返回，没有数据就设置后返回.
// loaded true返回的是当前已有的数据；false，参数给定的数据
func (m *Map) LoadOrStore(key, value interface{}) (actual interface{}, loaded bool) {
	// Avoid locking if it's a clean hit.
	read, _ := m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok {
		actual, loaded, ok := e.tryLoadOrStore(value)
		if ok {
			return actual, loaded
		}
	}

	m.mu.Lock()
	read, _ = m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok {
		if e.unexpungeLocked() {
			m.dirty[key] = e
		}
		actual, loaded, _ = e.tryLoadOrStore(value)
	} else if e, ok := m.dirty[key]; ok {
		actual, loaded, _ = e.tryLoadOrStore(value)
		m.missLocked()
	} else {
		if !read.amended {
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			m.dirtyLocked()
			m.read.Store(readOnly{m: read.m, amended: true})
		}
		m.dirty[key] = newEntry(value)
		actual, loaded = value, false
	}
	m.mu.Unlock()

	return actual, loaded
}

// tryLoadOrStore atomically loads or stores a value if the entry is not
// expunged.
//
// If the entry is expunged, tryLoadOrStore leaves the entry unchanged and
// returns with ok==false.
//
// tryLoadOrStore 在entry 为非expunged的时候，加载或存储一个值
//
// 如果entry 为 expunged，tryLoadOrStore什么都不改变，然后返回ok==false
func (e *entry) tryLoadOrStore(i interface{}) (actual interface{}, loaded, ok bool) {
	p := atomic.LoadPointer(&e.p)
	if p == expunged {
		return nil, false, false
	}
	if p != nil {
		return *(*interface{})(p), true, true
	}

	// Copy the interface after the first load to make this method more amenable
	// to escape analysis: if we hit the "load" path or the entry is expunged, we
	// shouldn't bother heap-allocating.
	// +-? 这里不知道它在说什么？
	// 在第一次load之后拷贝接口，使得这个方法经得起逃避分析：如果处于 load 方法，或者
	// entry 是expunged， 我们不应该打断堆分配。
	// PS: [逃逸分析](https://zh.wikipedia.org/wiki/%E9%80%83%E9%80%B8%E5%88%86%E6%9E%90)
	//     个人理解：如果指针只在本函数使用，则需要分配到栈上，否则需要分配到堆上
	ic := i
	for {
		if atomic.CompareAndSwapPointer(&e.p, nil, unsafe.Pointer(&ic)) {
			return i, false, true
		}
		p = atomic.LoadPointer(&e.p)
		if p == expunged {
			return nil, false, false
		}
		if p != nil {
			return *(*interface{})(p), true, true
		}
	}
}

// Delete deletes the value for a key.
func (m *Map) Delete(key interface{}) {
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]
	if !ok && read.amended {
		m.mu.Lock()
		read, _ = m.read.Load().(readOnly)
		e, ok = read.m[key]
		if !ok && read.amended {
			delete(m.dirty, key)
		}
		m.mu.Unlock()
	}
	if ok {
		e.delete()
	}
}

func (e *entry) delete() (hadValue bool) {
	// delete 的时候，又有重新的Store，是否会有问题？
	// 不会的，因为这个entry相当于已经从map中删除了，重新添加的是新的entry
	// 真的是这样吗？
	// +- 在Store的时候也是更改entry中的指针，所以如果Store和Delete并发执行，Delete可能会删除Store新添加的数据
	for {
		p := atomic.LoadPointer(&e.p)
		if p == nil || p == expunged {
			return false
		}
		if atomic.CompareAndSwapPointer(&e.p, p, nil) {
			return true
		}
	}
}

// Range calls f sequentially for each key and value present in the map.
// If f returns false, range stops the iteration.
//
// Range does not necessarily correspond to any consistent snapshot of the Map's
// contents: no key will be visited more than once, but if the value for any key
// is stored or deleted concurrently, Range may reflect any mapping for that key
// from any point during the Range call.
//
// Range may be O(N) with the number of elements in the map even if f returns
// false after a constant number of calls.
//
// Range 为map中的每个key、value不断地调用函数f。如果f返回false，range停止迭代。
//
// Rang 并不对应Map内容的任何一致快照：不会有key会被访问超过一次，但是如果key的value
// 被更改或删除，则在Range的过程中，可能反映出任何点key对应的任何映射。
// +- 在遍历过程中，key的值可能会改变，Range也不能保证获取到这个改变。
//
func (m *Map) Range(f func(key, value interface{}) bool) {
	// We need to be able to iterate over all of the keys that were already
	// present at the start of the call to Range.
	// If read.amended is false, then read.m satisfies that property without
	// requiring us to hold m.mu for a long time.
	read, _ := m.read.Load().(readOnly)
	if read.amended {
		// m.dirty contains keys not in read.m. Fortunately, Range is already O(N)
		// (assuming the caller does not break out early), so a call to Range
		// amortizes an entire copy of the map: we can promote the dirty copy
		// immediately!
		m.mu.Lock()
		read, _ = m.read.Load().(readOnly)
		if read.amended { // 如果read中的数据不是全量，则将dirty赋值给read
			read = readOnly{m: m.dirty} // +- 直接将dirty赋值给read，不错！
			m.read.Store(read)
			m.dirty = nil
			m.misses = 0
		}
		m.mu.Unlock()
	}

	for k, e := range read.m {
		v, ok := e.load()
		if !ok {
			continue
		}
		if !f(k, v) {
			break
		}
	}
}

// 调用这个函数之前，必须先要加锁
func (m *Map) missLocked() {
	m.misses++
	if m.misses < len(m.dirty) {
		return
	}
	// 由于m.dirty是全量，这里可以直接替换m.read
	// 这样赋值之后，read.amended=false,也就是read是全量的
	m.read.Store(readOnly{m: m.dirty})
	m.dirty = nil
	m.misses = 0
}

// dirtyLocked 将read中的数据复制到dirty
// 已经删除（也就是为nil）的数据设置为expunged
func (m *Map) dirtyLocked() {
	if m.dirty != nil {
		return
	}

	read, _ := m.read.Load().(readOnly)
	m.dirty = make(map[interface{}]*entry, len(read.m))
	for k, e := range read.m {
		if !e.tryExpungeLocked() {
			m.dirty[k] = e
		}
	}
}

func (e *entry) tryExpungeLocked() (isExpunged bool) {
	p := atomic.LoadPointer(&e.p)
	for p == nil {
		if atomic.CompareAndSwapPointer(&e.p, nil, expunged) {
			return true
		}
		p = atomic.LoadPointer(&e.p)
	}
	return p == expunged
}
