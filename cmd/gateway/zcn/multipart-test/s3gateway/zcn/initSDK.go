package zcn

import (
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

var configDir string
var AllocationID string
var nonce int64

func InitializeSDK(configDir, allocid string, nonce int64) error {
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
		allocBytes, err := ioutil.ReadFile(allocFile)
		if err != nil {
			return err
		}

		AllocationID = strings.ReplaceAll(string(allocBytes), " ", "")
		AllocationID = strings.ReplaceAll(AllocationID, "\n", "")

		if len(AllocationID) != 64 {
			return fmt.Errorf("allocation id has length %d, should be 64", len(AllocationID))
		}
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
