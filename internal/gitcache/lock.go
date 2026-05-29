package gitcache

import "sync"

// keyedMutex provides per-key mutual exclusion. Operations on the same key are
// serialized; operations on distinct keys proceed concurrently. It is used to
// serialize fetch/worktree work per repository (keyed by PRRef.Key()).
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// newKeyedMutex returns a ready-to-use keyedMutex.
func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*sync.Mutex)}
}

// Lock acquires the lock for key and returns a function that releases it.
// Callers typically do: unlock := km.Lock(key); defer unlock().
//
// The per-key mutexes are retained for the lifetime of the keyedMutex. The set
// of repositories a service touches is bounded, so this never grows unbounded
// in practice and avoids the races inherent in reference-counted cleanup.
func (k *keyedMutex) Lock(key string) (unlock func()) {
	k.mu.Lock()
	m, ok := k.locks[key]
	if !ok {
		m = &sync.Mutex{}
		k.locks[key] = m
	}
	k.mu.Unlock()

	m.Lock()
	return m.Unlock
}
