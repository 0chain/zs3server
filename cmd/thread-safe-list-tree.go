package cmd

import (
	"sync"

	art "github.com/plar/go-adaptive-radix-tree"
)

type ThreadSafeListTree struct {
	tree art.Tree
	mu   sync.RWMutex
}

func newThreadSafeListTree() *ThreadSafeListTree {
	return &ThreadSafeListTree{tree: art.New()}
}

func (t *ThreadSafeListTree) Insert(key art.Key, value art.Value) (oldValue art.Value, updated bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tree.Insert(key, value)
}

func (t *ThreadSafeListTree) Delete(key art.Key) (value art.Value, deleted bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tree.Delete(key)
}

func (t *ThreadSafeListTree) ForEachPrefix(keyPrefix art.Key, callback art.Callback) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	t.tree.ForEachPrefix(keyPrefix, callback)
}
