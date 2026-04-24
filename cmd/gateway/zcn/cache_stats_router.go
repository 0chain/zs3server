// Copyright (c) 2026 0chain.
//
// Admin cache-stats endpoint. Returns per-tier hit counters plus the live
// per-tier enable state. Used during benchmarks to verify that reads are
// actually being served by the expected cache tier (tmpfs vs spillover vs
// blobber) rather than trusting the config alone.
//
// Usage:
//   GET /internal/cache_stats
//
// Response:
//   200 {
//     "tmpfs_hits":     <int>,
//     "spillover_hits": <int>,
//     "blobber_reads":  <int>,
//     "nfs_tmpfs_hits":      <int>,
//     "nfs_spillover_hits":  <int>,
//     "nfs_prewarm_fetches": <int>,
//     "tmpfs_enabled":     <bool>,
//     "spillover_enabled": <bool>,
//     "cache_disabled":    <bool>,
//     "nfs_sync_enabled": <bool>
//   }
package zcn

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"github.com/gorilla/mux"
	minio "github.com/minio/minio/cmd"
	"github.com/minio/minio/internal/logger"
)

type cacheStatsResp struct {
	TmpfsHits         int64            `json:"tmpfs_hits"`
	SpilloverHits     int64            `json:"spillover_hits"`
	BlobberReads      int64            `json:"blobber_reads"`
	NFSTmpfsHits      int64            `json:"nfs_tmpfs_hits"`
	NFSSpilloverHits  int64            `json:"nfs_spillover_hits"`
	NFSPrewarmFetches int64            `json:"nfs_prewarm_fetches"`
	TmpfsEnabled      bool             `json:"tmpfs_enabled"`
	SpilloverEnabled  bool             `json:"spillover_enabled"`
	CacheDisabled     bool             `json:"cache_disabled"`
	NFSSyncEnabled    bool             `json:"nfs_sync_enabled"`
	Prefetch          map[string]int64 `json:"prefetch,omitempty"`
	WriteThrough      map[string]any   `json:"write_through,omitempty"`
}

func cacheStatsHandler(w http.ResponseWriter, r *http.Request) {
	resp := cacheStatsResp{
		TmpfsHits:         atomic.LoadInt64(&tmpfsHitCount),
		SpilloverHits:     atomic.LoadInt64(&spilloverHitCount),
		BlobberReads:      atomic.LoadInt64(&blobberReadCount),
		NFSTmpfsHits:      atomic.LoadInt64(&nfsTmpfsHitCount),
		NFSSpilloverHits:  atomic.LoadInt64(&nfsSpilloverHitCount),
		NFSPrewarmFetches: atomic.LoadInt64(&nfsPrewarmFetchCount),
		TmpfsEnabled:      serverConfig.NFSTmpfsCacheEnabled,
		SpilloverEnabled:  serverConfig.NFSSpilloverCacheEnabled,
		CacheDisabled:     serverConfig.NFSCacheDisabled,
		NFSSyncEnabled:    serverConfig.NFSSyncEnabled,
		Prefetch:          PrefetchStatsSnapshot(),
		WriteThrough:      WriteThroughStatsSnapshot(),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// RegisterCacheStatsRouter wires GET /internal/cache_stats on the router.
func RegisterCacheStatsRouter(router *mux.Router) {
	logger.Info("cache-stats router init at /internal/cache_stats")
	router.Methods(http.MethodGet).Path("/internal/cache_stats").HandlerFunc(cacheStatsHandler)
}

func init() {
	minio.GatewayExtraRouters = append(minio.GatewayExtraRouters, RegisterCacheStatsRouter)
}
