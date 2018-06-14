## Cond

Connd 用于等待通知的场景，他有以下三个方法：

* `Wait` 等待信号
* `Sign` 发送信号
* `Broadcast` 唤醒所有的等待

创建Cond的时候需要传递一个Locker，用于同步Cond内部状态。在调用Cond的方法的时候，需要先锁定，然后再调用。

### Sign

```
c.L.Lock()
for !condition() { //<--这里使用for循环，在唤醒之后重新检查状态
    c.Wait()
}
... make use of condition ...
c.L.Unlock()
```

通知协程可以这样写

```
//L.Lock()  <-- 这里需要先锁定? 不用锁定
c.Sign()
//L.Unlock()
```

## Broadcast

在使用Broadcast的场景，则可以使用`RWMutex`替换`Mutex`，提高唤醒的并发度。

```
var n int
flag.IntVar(&n, "n", 2, "wait num")
flag.Parse()
var m sync.RWMutex
c := sync.NewCond(m.RLocker())
running := make(chan int, n)
awake := make(chan int, n)
exit := false
for i := 0; i < n; i++ {
    go func(g int) {
        c.L.RLock() // <-- 这里调用RLock，多个goroutine就可以同时执行。也就是说唤醒之后他们可以同时执行内容。
        for !exit {
            running <- g
            c.Wait()
            awake <- g
        }
        c.L.RUnlock()
    }(i)
}
for i := 0; i < n; i++ {
    <-running // Will deadlock unless n are running.
}
exit = true
//c.L.Lock() <-- 不用锁定
c.Broadcast()
//c.L.Unlock()
seen := make([]bool, n)
start := time.Now()
for i := 0; i < n; i++ {
    g := <-awake
    if seen[g] {
        fmt.Println("goroutine woke up twice")
    }
    seen[g] = true
}
dur := time.Since(start)
fmt.Println("brocadcast ok, use ", dur)
```

### 遗留问题

1. Cond为什么要传递一个Locker，而不是内部创建？

  这个锁并不是为了保证Cond内部更改的原子性，而是为了保证对条件更改的原子性。代码如下（详细代码参见TestRace）：

  1. go func() {
  2.   for sign != 2 {
  3.     c.Wait()
  4.   }
  5. }
  6. sign = 2
  7. c.Broadcast()
  如果代码执行到2之后，接着执行了6、7行，则第3行的c.Wait()将永远不会执行。所以我们需要使用Lock将信号改变和状态检查捆绑到一起。
  结论：如果在发送Sign或Broadcast的时候，没有涉及到信号改变，则不用对Sign和Broadcast加锁；如果改变的时候涉及到信号改变则需要加锁。

2. 在调用Sign和Wait的时候，注释收可以不用锁定，是否会有问题？

  参见如上的结论

## Map

Map 是一个并发的map，并实现了`amortized-constant-time`的`load`、`store`和`delete`。多个goroutine并发访问Map的方法是安全的。

它适用于如下场景：

1. 并发的遍历
2. 很少写入

**PS:** 注释原文如下，我的理解只能翻译出这些了

> It is optimized for use in concurrent loops with keys that are
> stable over time, and either few steady-state stores, or stores
> localized to one goroutine per key.

Map的实现原理是使用两个变量：read、dirty。read、dirty都存储一个map，其中map的value都是指向entry变量，从而保证两个map访问的数据是一个。read的map是不可以更改的（对entry内容的更改并不会改变read中的map），有写入的时候，先写入dirty，并进行标记（read.amended）。当对只存在dirty中变量的访问累计到一定量（Map.misses），或者有遍历（Rang）请求的时候，则使用dirty替代read中的map。替代过程是简单粗暴的，直接将dirty赋值给read，所以时间是可以忽略。

### 常用方法

* Store(key, value interface{})
* Delete(key interface{})
* Load(key interface{}) (value interface{}, ok bool)
* LoadOrStore(key, value interface{}) (actual interface{}, loaded bool)
  
  如果存在则Load value，否则会设置value，并返回新设置的value

* Range(f func(key, value interface{})

以上方法中

* `Load`和`LoadOrStore`在Map.misses的时候会重新创建一个map，并将read中的数据都拷贝到新的map中，所以其最坏时间可能是O(n)。
* `Range`在遍历的过程中，`entry`可能会更改，但是`Range`并不能保证捕获到这个更改。

### 关键代码分析

首先我们来看一下Map的结构

```
Map
  |- read
  |    |- m       map[interface{}]*entry
  |    |- amended bool 是否与dirty一致
  |- dirty map[interface{}]*entry
  |- misses int 从dirty中取数据的次数
entry
  |- p unsafe.Pointer // *interface{}
```

这样设计有如下的优点：

1. 通过read和dirty应对不同场景，read应对热点读取，dirty用于全量存储。在有遍历任务的时候，则更新read。
2. 将实际数据隔离，并不是在map中直接存储value，而是通过entry进行封装，这样的好处是read和dirty可以指向同一个entry，在更改的时候也只需要更改entry就可以，而不用分别更新read和dirty。entry.p会有如下的状态：
  
  * `nil`: entry已经被删除，并且m.dirty==nil
  * `expunged`: entry已经被删除，m.dirty!=nil，entry不在m.dirty中。
  * `其它`: entry是正常的存储在m.read.m[key]中，如果m.dirty!=nil也存在于m.dirty[key]
  
  其状态转换如下:

  * `其它     --> nil     `: 元素被删除的时候变为nil
  * `nil      --> expunged`: `tryExpungeLocked` 在拷贝`m.read` 到`m.dirty`时，已经删除的数据设置为`expunged`
  * `expunged --> nil     `: `unexpungeLocked` 重新设置已经删除的数据

### 参考文献

* [What is amortized time?](https://mortoray.com/2014/08/11/what-is-amortized-time/)
* [Constant Amortized Time](https://stackoverflow.com/questions/200384/constant-amortized-time/38261380)
* [逃逸分析](https://zh.wikipedia.org/wiki/%E9%80%83%E9%80%B8%E5%88%86%E6%9E%90)  个人理解：如果指针只在本函数使用，则需要分配到栈上，否则需要分配到堆上

## Mutex

为了保证锁的公平性，Mutex提供了两种调度模式

* `normal`：等待者按照FIFO排队，但是被唤醒的等待者会与新到达的 goroutines 进行竞争由于可能有很多新到达的等待者，并且它们已经运行在一个CPU上，所以被唤醒的很有可能失败。失败后会排在队列头部。如果这种等待超过1ms，则切换到饥饿模式。
* `starvation`: 在饥饿模式中，锁直接从解锁的goroutine传递给队列头部的等待者。
即使它们到达的时候，锁刚好释放。他们也不会进入spin（自旋）状态。它们会被放到队列的尾部。

如果一个等待者获取锁后发现如下情况，则会将锁切换回 normal 模式

1. 它是队列中最后一个等待者
2. 它等待的时间少于 1ms

`normal` 模式会有更高的性能，因为即使有很多的等待者，一个 goroutine 也可以多次获得锁。

`starvation` 模式则可以解决队尾长时间无法调度的问题

判断当前状态是已经加锁并且不在 `Starve` 模式

`runtime_canSpin` 条件

  1. 次数限制，当前为4次
  2. 多核
  3. GOMAXPROCS>1 并且至少有一个其它正在运行的 P
  4. 本地 runq 为空

## Once

对于一个`Once`实例，`Do()` 只会在第一次调用的时候执行 `f()` 。也就是说给定一个

```
  var once Once
```

如果 `once.Do(f)` 被调用多次，只有第一次会调用 `f()` ，即使在每次调用中 `f()` 有不同的值。`Once` 是以实例计算的。

`Do()` 预期是执行那些只会执行一次的初始化。由于 `f()` 并没有参数，对于需要参数的初始化，则需要在外层封装一下。

```
  config.once.Do(func() { config.init(filename) })
```

因为只有在 `f()` 返回之后 `Do()` 才能返回，如果 `f()` 中又调用 `Do()` 方法，则会导致死锁。

如果 `f()` panic，`Do` 会认为它已经返回，以后对 `Do` 的调用会直接返回，并不会调用 `f` 。

在调用`Do`的时候，会首先判断是否调用过，从而快速判断，快速返回。

## Pool

`Pool` 是单独被保存和检索的临时对象集合。

所有存储在`Pool`中的`item`可能随时自动删除，而没有任何通知。如果只有`Pool`中持有`item`引用，则item可能被释放（deallocated）。

`Pool` 在多个`goroutine`中的使用是安全的。

`Pool` 的目的是缓存已经分配但未使用的 `item`，以备后用，以减轻垃圾回收的压力。也就是说，它可以轻松构建高效的、线程安全的空闲列表（free list）。但是，它并不适合所有的空闲列表。

`Pool` 适用于管理一组临时 `item`，他们可能被包（package）下的不同的客户端（client）悄悄的共享并可能被重用。`Pool`提供了一种方法，缓解跨多个客户端的分配（allocation）开销。

`fmt`包中有一个`Pool`的很好使用的例子，它维护了一个动态大小的临时输出缓冲（buffer）的存储区（store）。这个存储区在很多goroutine活跃打印的时候扩大，在不活跃的时候进行收缩。

另一方面，对短期对象的一部分而维护的空闲列表是不适合使用`Pool`的，因为在该场景中开销并不能很好地被分摊。让这些对象实现自己的空闲列表会更高效。

`Pool`在使用之后不允许拷贝。

`Pool`主要提供两个方法：

* `Put`：锁定核不被抢占之后，首先尝试设置`private`，然后添加到`shared`中。
* `Get`：由于`Pool`是与每个核进行绑定的，所以这里只要保证每个核只有一个人获取`private`就可以，对于`shared`访问则由锁进行保证正确性。

以下两个函数可以保证不被抢占：

`func runtime_procPin() int`: 锁定这个核，不被抢占
`func runtime_procUnpin()`：释放锁定

通过`func runtime_registerPoolCleanup(cleanup func())`注册清理函数，在GC的过程中清理`Pool`中的内容。


**TODO:** 看看fmt中如何使用Pool









-----
