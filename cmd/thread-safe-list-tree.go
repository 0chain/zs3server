package cmd

import (
	"sync"
	"time"

	"github.com/armon/go-radix"
)

type ThreadSafeListTree struct {
	tree *radix.Tree
	mu   sync.RWMutex
}

func newThreadSafeListTree() *ThreadSafeListTree {
	return &ThreadSafeListTree{tree: radix.New()}
}

func (t *ThreadSafeListTree) Insert(key string, value any) (any, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tree.Insert(key, value)
}

func (t *ThreadSafeListTree) Delete(key string) (any, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tree.Delete(key)
}

func (t *ThreadSafeListTree) ForEachPrefix(keyPrefix string, callback radix.WalkFn) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	t.tree.WalkPrefix(keyPrefix, callback)
}

func (t *ThreadSafeListTree) Get(key string) (any, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.tree.Get(key)
}

func (t *ThreadSafeListTree) CheckTimeAndDelete(key string, time time.Time) (any, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	value, ok := t.tree.Get(key)
	if !ok {
		return nil, false
	}
	v := value.(ObjectInfo)
	if v.ModTime.UnixNano() == time.UnixNano() {
		return t.tree.Delete(key)
	}
	return nil, false
}
