package zcn

import (
	"encoding/json"
	"fmt"
	"github.com/0chain/gosdk/core/client"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/0chain/gosdk/core/conf"
	"github.com/0chain/gosdk/core/logger"
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
	if err == nil {
		err = json.Unmarshal(optionBytes, &serverConfig)
		if err != nil {
			return err
		}
	}
	encrypt = serverConfig.Encrypt
	compress = serverConfig.Compress
	if serverConfig.MaxBatchSize == 0 {
		serverConfig.MaxBatchSize = 25
		serverConfig.BatchWorkers = 5
		serverConfig.BatchWaitTime = 500
	} else if serverConfig.BatchWorkers == 0 {
		serverConfig.BatchWorkers = 5
	} else if serverConfig.BatchWaitTime == 0 {
		serverConfig.BatchWaitTime = 500
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

	logger.SyncLoggers([]*logger.Logger{zcncore.GetLogger(), sdk.GetLogger()})
	zcncore.SetLogFile("cmdlog.log", true)
	sdk.SetLogFile("cmd.log", true)
	zcncore.SetLogLevel(3)
	sdk.SetLogLevel(3)

	err = client.InitSDK(string(walletBytes), cfg.BlockWorker, cfg.ChainID, cfg.SignatureScheme, nonce, false, true, cfg.MinSubmit, cfg.MinConfirmation, cfg.ConfirmationChainLength, cfg.SharderConsensous)
	if err != nil {
		return err
	}

	conf.InitClientConfig(&cfg)

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
