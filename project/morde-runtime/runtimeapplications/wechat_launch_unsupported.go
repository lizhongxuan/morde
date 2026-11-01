//go:build !linux

package runtimeapplications

import (
	"context"
	"os/exec"
)

func wechatLaunchArchitectureSupported() bool { return false }

func prepareWechatLaunch(context.Context) (*exec.Cmd, uint32, uint32, string) {
	return nil, 0, 0, "architecture_unsupported"
}
