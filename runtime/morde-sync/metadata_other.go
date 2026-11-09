//go:build !darwin && !linux && !windows

package mordesync

import (
	"fmt"
	"os"
)

func captureMetadataFD(_ *os.File, name string) ([]ExtendedAttribute, *NativeACL, error) {
	return nil, nil, fmt.Errorf("%w: ACL/extended-attribute inspection is unavailable for %q on this platform", ErrUnsupportedMetadata, name)
}

func captureSymlinkMetadataAt(_ *os.File, _ string, relative string) ([]ExtendedAttribute, *NativeACL, error) {
	return nil, nil, fmt.Errorf("%w: symlink ACL/extended-attribute inspection is unavailable for %q on this platform", ErrUnsupportedMetadata, relative)
}

func applyMetadataFD(_ *os.File, name string, _ []ExtendedAttribute, _ *NativeACL, _ bool) error {
	return fmt.Errorf("%w: ACL/extended-attribute restore is unavailable for %q on this platform", ErrUnsupportedMetadata, name)
}

func applySymlinkMetadataAt(_ *os.File, _ string, relative string, _ []ExtendedAttribute, _ *NativeACL, _ bool) error {
	return fmt.Errorf("%w: symlink ACL/extended-attribute restore is unavailable for %q on this platform", ErrUnsupportedMetadata, relative)
}
