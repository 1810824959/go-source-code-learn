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
type Mutex struct {
	// state，锁的状态，就是下面的mutexLocked等
	// 默认是零值，所以是0，没锁的状态
	// int32，那就是32位，8位一个字节，那就是4个字节，低三位是状态，后面都代表等待的协程个数
	state int32
	// 信号量
	// 操作系统里的PV操作，操作的就是信号量
	sema uint32
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	// 001 加锁状态
	mutexLocked = 1 << iota // mutex is locked
	// 010 锁上有被唤醒的协程
	mutexWoken
	// 100 饥饿状态
	mutexStarving
	// 3，上面可以看到，低三位都被用于标识状态了，所以这里就表示
	// state需要右移几位，才能真正表示个数，这里是3
	mutexWaiterShift = iota

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

	// 1ms
	starvationThresholdNs = 1e6
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.

	// 先简单尝试CAS
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// Slow path (outlined so that the fast path can be inlined)
	// 不行就走这里
	m.lockSlow()
}

// TryLock tries to lock m and reports whether it succeeded.
//
// Note that while correct uses of TryLock do exist, they are rare,
// and use of TryLock is often a sign of a deeper problem
// in a particular use of mutexes.
func (m *Mutex) TryLock() bool {
	old := m.state
	if old&(mutexLocked|mutexStarving) != 0 {
		return false
	}

	// There may be a goroutine waiting for the mutex, but we are
	// running now and can try to grab the mutex before that
	// goroutine wakes up.
	if !atomic.CompareAndSwapInt32(&m.state, old, old|mutexLocked) {
		return false
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
	return true
}

func (m *Mutex) lockSlow() {
	var waitStartTime int64
	starving := false
	awoke := false
	iter := 0
	old := m.state

	// 进入拿锁的大循环
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.

		// 尝试自旋
		// mutexLocked|mutexStarving = 011 ，所以此时old只能是001
		// 且canSpin
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.

			// 如果没有被唤醒，却state不是唤醒中，就把锁变成唤醒
			// 这样的话，unlock的时候，就会直接return，而不是唤醒其他goroutine
			// 上面的英文就是这个意思
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			runtime_doSpin()
			iter++
			old = m.state
			continue
		}

		// 根据锁的不同状态，计算出锁的新状态
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		//  & 100 = 0，说明要么是001，要么是010，反正不是饥饿
		// 那就尝试性加上锁，先不管加不加得上，如果是饥饿，就算了，饥饿时，新来的
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}
		// mutexLocked|mutexStarving = 101
		// 这里不为0，说明肯定有个1
		// 100,001，饥饿，或者上锁状态
		// 此时 wait左移一位，说明排队去
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.

		// 如果此时g是饥饿，old&001!=0，所以old一定是加锁状态
		// 就将new 变成饥饿，后面把锁变成饥饿
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken
		}

		// 更新锁状态，更新成功，说明old没变，暂时没有别的协程获得锁
		// 失败就进入else，从头开始for循环，又从检查自旋开始
		if atomic.CompareAndSwapInt32(&m.state, old, new) {

			// x & y = 0，说明不是
			// mutexLocked|mutexStarving 是 101 ，old&101=0，说明是000，或者010
			// 那就直接加锁
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}

			// 到这里就是没拿到锁的情况了
			// If we were already waiting before, queue at the front of the queue.

			// 注释说了，是第二次进来的话，就放在队头
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)

			// 如果等了超过1ms，协程就开始饥饿
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs

			// 在得到信号量之后，获取一下新的状态
			old = m.state

			// 饥饿状态就进来
			if old&mutexStarving != 0 {
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				// delta没看懂，算了，也无伤大雅
				delta := int32(mutexLocked - 1<<mutexWaiterShift)

				// 两个条件就mutex就退出饥饿
				// 1）经过前面的计算，当前goroutine已经不是饥饿，等待小于1ms
				// 2）old>> 之后只剩1，说明队列中只有自己一个，那就也退出饥饿
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
			// 唤醒，进入下一轮，去获得锁
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
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}

	// 如果不是饥饿
	if new&mutexStarving == 0 {
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.

			// 首先检查能不能直接返回，不用刻意去唤醒
			// 1）队列里没有等待的g   2）锁此时有状态，说明有别人在操作
			// 锁着的，说明有人拿到了；唤醒状态，说明有人马上拿到了；饥饿着，说明有人马上要执行handoff的唤醒
			// 这些情况都不用唤醒，直接返回
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.

			// 这里就尝试唤醒，发送信号
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else {
		// 进入这里，就是饥饿模式
		// 此时把mutex唤醒方式变成handoff，直接交给队伍头部第一个
		// 而现在的第一个是什么成分的goroutine呢，是被唤醒后，仍然没有拿到锁的goroutine
		// 有可能是饥饿，也有可能不是，但不管是不是，都给他了
		// 但此时并不解除饥饿，什么时候解除呢？逻辑在加锁那里
		// 饥饿模式下，goroutine拿到锁（只可能是队头），检查得到锁小于1ms，或者队伍只剩下自己了
		// state就退出饥饿

		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		runtime_Semrelease(&m.sema, true, 1)
	}
}
