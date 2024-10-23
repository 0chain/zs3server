package cmd

import (
	"sync"

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
