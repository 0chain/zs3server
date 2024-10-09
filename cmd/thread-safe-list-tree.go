package cmd

import (
	"sync"

	art "github.com/arriqaaq/art"
)

type ThreadSafeListTree struct {
	tree *art.Tree
	mu   sync.RWMutex
}

func newThreadSafeListTree() *ThreadSafeListTree {
	return &ThreadSafeListTree{tree: art.NewTree()}
}

func (t *ThreadSafeListTree) Insert(key []byte, value any) (updated bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tree.Insert(key, value)
}

func (t *ThreadSafeListTree) Delete(key []byte) (deleted bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tree.Delete(key)
}

func (t *ThreadSafeListTree) ForEachPrefix(keyPrefix []byte, callback art.Callback) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	t.tree.Scan(keyPrefix, callback)
}
