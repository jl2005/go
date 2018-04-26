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
2. 在调用Sign和Wait的时候，注释收可以不用锁定，是否会有问题？

















-----
