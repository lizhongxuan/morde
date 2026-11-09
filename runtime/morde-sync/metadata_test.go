package mordesync

import "testing"

func TestSameMetadataTreatsNilAndEmptyXattrsEqually(t *testing.T) {
	if !sameMetadata(nil, nil, []ExtendedAttribute{}, nil) || !sameMetadata([]ExtendedAttribute{}, nil, nil, nil) {
		t.Fatal("nil and empty xattr lists should represent the same metadata")
	}
}
