// Sequential access prefetch.
//
// When a client does GETs in lexicographic order within the same bucket/dir
// ("dataset/img_00001.JPEG", "dataset/img_00002.JPEG", ...), we predict
// the next few keys and spawn cacheBackFullFetch for them so they're hot by
// the time the client asks. This pipelines storage with compute — the
// single biggest AU lever on MLPerf training loops beyond raw MB/s.
//
// Heuristic: track the last N observed keys per (bucket, parent-dir). If
// they form a sorted increasing run, list the parent dir and prefetch the
// next `prefetchLookahead` keys. List is already done via ListObjectsV2
// with start-after = current key, MaxKeys = lookahead — one extra RPC
// per run-start amortised over the whole batch.

package zcn

import (
	"context"
	"path"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/0chain/gosdk/zboxcore/sdk"
)

const (
	prefetchWindow     = 3  // must see N consecutive sorted keys in same dir
	prefetchLookahead  = 4  // prefetch next N keys after detection
	prefetchMaxTrackers = 64 // LRU cap on per-dir trackers
)

type dirTracker struct {
	lastKeys []string // ring of the last `prefetchWindow` keys observed
	pos      int
	filled   bool
}

type prefetchPredictor struct {
	mu        sync.Mutex
	trackers  map[string]*dirTracker // key: bucket + "\x00" + parentDir
	order     []string               // LRU order for eviction
	ctx       context.Context
	alloc     *sdk.Allocation
	cacheBack func(alloc *sdk.Allocation, bucket, object, remotePath, cacheDir, cacheKey string)
	cacheDir  func() string

	// Stats
	hits        atomic.Int64
	predictions atomic.Int64
	dispatched  atomic.Int64
}

var globalPrefetch *prefetchPredictor

// InitPrefetchPredictor wires the predictor with the allocation and the
// cacheBackFullFetch function. Call once at gateway startup, after
// currentAlloc is set.
func InitPrefetchPredictor(alloc *sdk.Allocation,
	cacheBack func(alloc *sdk.Allocation, bucket, object, remotePath, cacheDir, cacheKey string),
	cacheDirFn func() string) {
	globalPrefetch = &prefetchPredictor{
		trackers:  make(map[string]*dirTracker),
		ctx:       context.Background(),
		alloc:     alloc,
		cacheBack: cacheBack,
		cacheDir:  cacheDirFn,
	}
}

// RecordGet is called from the GET handler after serving a response. It
// updates the per-dir tracker and, if a sequential run is detected, kicks
// off cacheBackFullFetch for the next few keys. Non-blocking.
func RecordGet(bucket, object string) {
	if globalPrefetch == nil {
		return
	}
	globalPrefetch.recordGet(bucket, object)
}

func (p *prefetchPredictor) recordGet(bucket, object string) {
	parent := path.Dir(object)
	trackerKey := bucket + "\x00" + parent

	p.mu.Lock()
	t, ok := p.trackers[trackerKey]
	if !ok {
		if len(p.trackers) >= prefetchMaxTrackers {
			evict := p.order[0]
			p.order = p.order[1:]
			delete(p.trackers, evict)
		}
		t = &dirTracker{lastKeys: make([]string, prefetchWindow)}
		p.trackers[trackerKey] = t
		p.order = append(p.order, trackerKey)
	}
	t.lastKeys[t.pos] = object
	t.pos = (t.pos + 1) % prefetchWindow
	if t.pos == 0 {
		t.filled = true
	}
	// Copy out under lock, release, then decide
	copyKeys := append([]string(nil), t.lastKeys...)
	filled := t.filled
	p.mu.Unlock()

	if !filled {
		return
	}
	if !isSortedRun(copyKeys, t.pos) {
		return
	}
	p.hits.Add(1)
	go p.dispatchPrefetch(bucket, parent, object)
}

// isSortedRun returns true if the ring buffer is in strictly-increasing
// order. `pos` is the index just past the newest entry, so we unroll in
// logical order starting at pos.
func isSortedRun(ring []string, pos int) bool {
	n := len(ring)
	for i := 0; i < n-1; i++ {
		a := ring[(pos+i)%n]
		b := ring[(pos+i+1)%n]
		if a >= b {
			return false
		}
	}
	return true
}

// dispatchPrefetch lists the parent dir for next keys after currentKey
// and kicks cacheBackFullFetch for each. Fires in a goroutine.
func (p *prefetchPredictor) dispatchPrefetch(bucket, parentDir, afterKey string) {
	if p.alloc == nil || p.cacheBack == nil {
		return
	}
	cacheDir := p.cacheDir()
	if cacheDir == "" {
		return
	}
	nextKeys := p.listNextKeys(bucket, parentDir, afterKey, prefetchLookahead)
	p.predictions.Add(int64(len(nextKeys)))
	for _, k := range nextKeys {
		// Skip if already hot (cheap stat check avoids a blobber round-trip)
		cacheKey := bucket + "/" + k
		if _, loaded := cacheBackInflight.LoadOrStore(cacheKey, true); loaded {
			continue
		}
		remotePath := "/"
		if bucket == rootBucketName {
			remotePath = remotePath + k
		} else {
			remotePath = remotePath + bucket + "/" + k
		}
		go p.cacheBack(p.alloc, bucket, k, remotePath, cacheDir, cacheKey)
		p.dispatched.Add(1)
	}
}

// listNextKeys returns the next N keys in `bucket/parentDir/` after
// `afterKey` in lexicographic order. Uses the gosdk allocation's GetRefs
// rather than MinIO's LIST to avoid the S3 handler round-trip.
func (p *prefetchPredictor) listNextKeys(bucket, parentDir, afterKey string, n int) []string {
	if p.alloc == nil {
		return nil
	}
	dirPath := "/" + bucket + "/" + parentDir
	if bucket == rootBucketName {
		dirPath = "/" + parentDir
	}
	dirPath = strings.TrimSuffix(dirPath, "/")
	// dStorage.go:175 pattern — level = components(dirPath)+1 to enumerate
	// immediate children. offsetPath is the full path of the current key
	// and acts as a lexicographic cursor (must include allocation-rooted
	// prefix, not just basename). fileType="f" filters to regular files.
	level := len(strings.Split(strings.TrimSuffix(dirPath, "/"), "/")) + 1
	offsetPath := "/" + bucket + "/" + afterKey
	if bucket == rootBucketName {
		offsetPath = "/" + afterKey
	}
	results, err := p.alloc.GetRefs(dirPath, offsetPath, "", "", "f", "regular", level, n)
	if err != nil || results == nil {
		return nil
	}
	keys := make([]string, 0, len(results.Refs))
	prefix := ""
	if parentDir != "." && parentDir != "/" && parentDir != "" {
		prefix = parentDir + "/"
	}
	for _, r := range results.Refs {
		if r.Type != "f" {
			continue
		}
		name := path.Base(r.Path)
		if name == "" {
			continue
		}
		keys = append(keys, prefix+name)
	}
	return keys
}

// PrefetchStatsSnapshot returns counters for monitoring.
func PrefetchStatsSnapshot() map[string]int64 {
	if globalPrefetch == nil {
		return map[string]int64{}
	}
	return map[string]int64{
		"prefetch_hits":        globalPrefetch.hits.Load(),
		"prefetch_predictions": globalPrefetch.predictions.Load(),
		"prefetch_dispatched":  globalPrefetch.dispatched.Load(),
	}
}
