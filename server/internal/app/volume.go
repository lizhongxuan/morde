package app

import (
	"context"
	"errors"
)

var ErrVolumeInUse = errors.New("volume is in use")

type VolumeDeleter interface {
	DeleteVolume(context.Context, string) error
}

type DiskCapacityChecker interface {
	CheckCapacity(context.Context) error
}
