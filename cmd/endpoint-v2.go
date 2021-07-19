// Copyright (c) 2015-2021 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"fmt"
	"strings"

	"github.com/minio/cli"
	"github.com/minio/pkg/ellipses"
)

var (
	globalMinioDrives string
)

const (
	driveSeparator = "drives="
)

func parseDrives(cctx *cli.Context) (string, error) {
	if !strings.Contains(cctx.Args().First(), driveSeparator) {
		return "", nil
	}
	tokens := strings.SplitN(cctx.Args().First(), driveSeparator, 2)
	if len(tokens) != 2 {
		return "", fmt.Errorf("unable to parse input args %s", cctx.Args())
	}
	drives := strings.TrimSpace(tokens[1])
	if !ellipses.HasEllipses(drives) {
		return "", fmt.Errorf("unable to parse input args %s, only allows ellipses patterns", cctx.Args())
	}
	return drives, nil
}
