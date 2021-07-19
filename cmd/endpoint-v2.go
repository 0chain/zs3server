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
	driveSeparator = "="
)

func parseDrives(cctx *cli.Context) (string, error) {
	tokens := strings.SplitN(cctx.Args().First(), driveSeparator, 2)
	if len(tokens) != 2 {
		return "", fmt.Errorf("unable to parse input args %s", cctx.Args())
	}
	if !ellipses.HasEllipses(tokens[1]) {
		return "", fmt.Errorf("unable to parse input args %s, only allows ellipses patterns", cctx.Args())
	}
	return tokens[1], nil
}
