package cmd

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/pkg/ellipses"
)

func expandDriveVolumeMeta(driveArg string) (drives []string, err error) {
	patterns, err := ellipses.FindEllipsesPatterns(driveArg)
	if err != nil {
		return nil, err
	}
	for _, lbls := range patterns.Expand() {
		driveAbs, err := filepath.Abs(strings.Join(lbls, ""))
		if err != nil {
			// fail for non-absolute paths
			return nil, err
		}
		drives = append(drives, driveAbs)
	}
	return drives, nil
}

type volumePool struct {
	ID      string `json:"id"`
	Local   string `json:"local"`
	Remote  string `json:"remote"`
	CmdLine string `json:"cmdline"`
}

type volumeMeta struct {
	Version string       `json:"version"`
	Token   string       `json:"token"`
	Pools   []volumePool `json:"pools"`
}

const volumeMetaFile = "volume.meta"

func checkDriveVolumeMeta(driveArg string) (*volumeMeta, error) {
	drives, err := expandDriveVolumeMeta(driveArg)
	if err != nil {
		return nil, err
	}
	volumeMetas := make([]*volumeMeta, len(drives))
	for i, drv := range drives {
		buf, err := ioutil.ReadFile(filepath.Join(drv, minioMetaBucket, volumeMetaFile))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if os.IsNotExist(err) {
			continue
		}
		var vm = new(volumeMeta)
		if err = json.Unmarshal(buf, vm); err != nil {
			return nil, err
		}
		volumeMetas[i] = vm
	}
	return volumeMetas[0], nil
}
