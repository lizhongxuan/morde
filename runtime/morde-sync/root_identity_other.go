//go:build !darwin && !linux && !windows

package mordesync

import "os"

func matchesSelectedRootIdentity(*os.Root, string, string) bool { return false }
