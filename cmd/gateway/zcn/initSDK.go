package zcn

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/0chain/gosdk/core/conf"
	"github.com/0chain/gosdk/core/logger"
	"github.com/0chain/gosdk/zboxcore/blockchain"
	"github.com/0chain/gosdk/zboxcore/sdk"
	"github.com/0chain/gosdk/zcncore"
	"github.com/mitchellh/go-homedir"
)

type serverOptions struct {
	Encrypt               bool `json:"encrypt"`
	Compress              bool `json:"compress"`
	MaxBatchSize          int  `json:"max_batch_size"`
	BatchWaitTime         int  `json:"batch_wait_time"`
	BatchWorkers          int  `json:"batch_workers"`
	UploadWorkers         int  `json:"upload_workers"`
	DownloadWorkers       int  `json:"download_workers"`
	MaxConcurrentRequests int  `json:"max_concurrent_requests"`
	SDKBatchSize          int  `json:"sdk_batch_size"`
	LockedBlobbersCap     int  `json:"locked_blobbers_cap"`
	EnableWAL             bool   `json:"enable_wal"`
	WALDir                string `json:"wal_dir"`
	WALCommitWorkers      int    `json:"wal_commit_workers"`
	EnableNFS             bool   `json:"enable_nfs"`
	NFSPort               int    `json:"nfs_port"`
	NFSCacheDir           string `json:"nfs_cache_dir"`
	NFSCacheMode          string `json:"nfs_cache_mode"`          // "tmpfs" (fastest), "nvme" (crash-safe), "direct" (sync blobber, slowest)
	NFSGaneshaExportDir   string `json:"nfs_ganesha_export_dir"` // NFS-Ganesha export directory
	NFSSyncWorkers        int    `json:"nfs_sync_workers"`       // blobber sync workers (default 8)
	NFSSpilloverDir       string `json:"nfs_spillover_dir"`      // NVMe spillover when tmpfs full
	NFSSpilloverMaxBytes  int64  `json:"nfs_spillover_max_bytes"` // cap on spillover dir size (0 = unlimited). At cap, oldest files evicted.
	NFSCacheEvict         bool   `json:"nfs_cache_evict"`        // delete from cache after blobber commit (default: true)
	NFSCacheBackEnabled   bool   `json:"nfs_cacheback_enabled"` // S3 GET read-miss cache-back to /nfs_export (default: false)
	NFSCacheDisabled      bool   `json:"nfs_cache_disabled"`    // worst-case baseline: skip Fix A + cacheBackFullFetch + cacheBackTee + TryCacheRead; all reads go direct to gosdk (default: false)
	NFSTmpfsCacheEnabled     bool `json:"nfs_tmpfs_cache_enabled"`     // per-tier gate: tmpfs / NFSGaneshaExportDir (default: true if absent from JSON)
	NFSSpilloverCacheEnabled bool `json:"nfs_spillover_cache_enabled"` // per-tier gate: spillover / NFSSpilloverDir (default: true if absent from JSON)
	NFSSyncEnabled           bool `json:"nfs_sync_enabled"`            // gate for inotify NFS-Sync watcher + processEvents/batch workers (default: true if absent from JSON)
	NFSDirectThreshold    int64  `json:"nfs_direct_threshold"`   // files above this size (bytes) bypass cache, write direct to blobber (default: 2MB, 0=disabled)
	S3DirectThreshold     int64  `json:"s3_direct_threshold"`    // same for S3 path (default: 0=disabled, all go through cache)

	// S3-upstream fallback (fetch missing objects from external S3, cache-back to Zus)
	FallbackS3Enabled   bool              `json:"fallback_s3_enabled"`
	FallbackS3Endpoint  string            `json:"fallback_s3_endpoint"`
	FallbackS3Region    string            `json:"fallback_s3_region"`
	FallbackS3AccessKey string            `json:"fallback_s3_access_key"`
	FallbackS3SecretKey string            `json:"fallback_s3_secret_key"`
	FallbackS3UseSSL    bool              `json:"fallback_s3_use_ssl"`
	FallbackBucketMap   map[string]string `json:"fallback_bucket_map"`
}

func initializeSDK(configDir, allocid string, nonce int64) error {
	if configDir == "" {
		var err error
		configDir, err = getDefaultConfigDir()
		if err != nil {
			return err
		}
	}

	if _, err := os.Stat(configDir); err != nil {
		return err
	}

	if allocid == "" {
		allocFile := filepath.Join(configDir, "allocation.txt")
		allocBytes, err := os.ReadFile(allocFile)
		if err != nil {
			return err
		}

		allocationID = strings.ReplaceAll(string(allocBytes), " ", "")
		allocationID = strings.ReplaceAll(allocationID, "\n", "")

		if len(allocationID) != 64 {
			return fmt.Errorf("allocation id has length %d, should be 64", len(allocationID))
		}
	}

	optionFile := filepath.Join(configDir, "zs3server.json")
	optionBytes, err := os.ReadFile(optionFile)
	// Default per-tier gates to true BEFORE unmarshal so absent keys stay on
	// (json.Unmarshal leaves fields at zero-value when absent, so we can't
	// distinguish "absent" from "explicit false" post-unmarshal without
	// peeking at the raw map). Policy: if the key is absent, keep default
	// true; if it is present, honour the JSON value (even if false).
	serverConfig.NFSTmpfsCacheEnabled = true
	serverConfig.NFSSpilloverCacheEnabled = true
	serverConfig.NFSSyncEnabled = true
	if err == nil {
		// First peek at raw keys so an explicit "false" in JSON wins over
		// our default-true. We re-unmarshal into serverConfig afterwards.
		var rawMap map[string]json.RawMessage
		if rerr := json.Unmarshal(optionBytes, &rawMap); rerr == nil {
			if v, ok := rawMap["nfs_tmpfs_cache_enabled"]; ok {
				var b bool
				if json.Unmarshal(v, &b) == nil {
					serverConfig.NFSTmpfsCacheEnabled = b
				}
			}
			if v, ok := rawMap["nfs_spillover_cache_enabled"]; ok {
				var b bool
				if json.Unmarshal(v, &b) == nil {
					serverConfig.NFSSpilloverCacheEnabled = b
				}
			}
			if v, ok := rawMap["nfs_sync_enabled"]; ok {
				var b bool
				if json.Unmarshal(v, &b) == nil {
					serverConfig.NFSSyncEnabled = b
				}
			}
		}
		// Save the default-aware per-tier flags, unmarshal the rest, then
		// restore them (plain Unmarshal would zero them out if absent).
		tmpfsOn := serverConfig.NFSTmpfsCacheEnabled
		spillOn := serverConfig.NFSSpilloverCacheEnabled
		syncOn := serverConfig.NFSSyncEnabled
		err = json.Unmarshal(optionBytes, &serverConfig)
		if err != nil {
			return err
		}
		serverConfig.NFSTmpfsCacheEnabled = tmpfsOn
		serverConfig.NFSSpilloverCacheEnabled = spillOn
		serverConfig.NFSSyncEnabled = syncOn
	}
	encrypt = serverConfig.Encrypt
	compress = serverConfig.Compress
	if serverConfig.MaxBatchSize == 0 {
		serverConfig.MaxBatchSize = 50
		serverConfig.BatchWorkers = 5
		serverConfig.BatchWaitTime = 500
	} else if serverConfig.BatchWorkers == 0 {
		serverConfig.BatchWorkers = 5
	} else if serverConfig.BatchWaitTime == 0 {
		serverConfig.BatchWaitTime = 500
	}
	if serverConfig.LockedBlobbersCap == 0 {
		serverConfig.LockedBlobbersCap = serverConfig.BatchWorkers
	}
	if serverConfig.MaxConcurrentRequests == 0 {
		serverConfig.MaxConcurrentRequests = serverConfig.MaxBatchSize
	}

	cfg, err := conf.LoadConfigFile(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		return err
	}

	walletFile := filepath.Join(configDir, "wallet.json")

	walletBytes, err := ioutil.ReadFile(walletFile)
	if err != nil {
		return err
	}

	network, _ := conf.LoadNetworkFile(filepath.Join(configDir, "network.yaml"))
	if network.IsValid() {
		zcncore.SetNetwork(network.Miners, network.Sharders)
		conf.InitChainNetwork(&conf.Network{
			Miners:   network.Miners,
			Sharders: network.Sharders,
		})
	}

	logger.SyncLoggers([]*logger.Logger{zcncore.GetLogger(), sdk.GetLogger()})
	zcncore.SetLogFile("cmdlog.log", true)
	sdk.SetLogFile("cmd.log", true)
	zcncore.SetLogLevel(3)
	sdk.SetLogLevel(3)

	err = zcncore.InitZCNSDK(cfg.BlockWorker, cfg.SignatureScheme,
		zcncore.WithChainID(cfg.ChainID),
		zcncore.WithMinSubmit(cfg.MinSubmit),
		zcncore.WithMinConfirmation(cfg.MinConfirmation),
		zcncore.WithConfirmationChainLength(cfg.ConfirmationChainLength))
	if err != nil {
		return err
	}

	err = sdk.InitStorageSDK(string(walletBytes), cfg.BlockWorker, cfg.ChainID, cfg.SignatureScheme, nil, nonce)
	if err != nil {
		return err
	}

	// Persist the allocation to disk after the first successful sharder
	// fetch so subsequent restarts (chain up or down) bootstrap from disk
	// without calling sharders. Mirrors the eblobber id-first protocol
	// patch: chain is needed only on first ever bring-up.
	sdk.SetAllocationCacheDir(filepath.Join(configDir, "alloc_cache"))

	blockchain.SetMaxTxnQuery(cfg.MaxTxnQuery)
	blockchain.SetQuerySleepTime(cfg.QuerySleepTime)
	conf.InitClientConfig(&cfg)

	initFallbackS3()

	if network.IsValid() {
		sdk.SetNetwork(network.Miners, network.Sharders)
	}

	sdk.SetNumBlockDownloads(10)
	return nil
}

func getDefaultConfigDir() (string, error) {
	homeDir, err := homedir.Dir()
	if err != nil {
		return "", err
	}

	configDir := filepath.Join(homeDir, ".zcn")

	return configDir, nil
}
