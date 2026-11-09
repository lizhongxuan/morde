package mordesync

import (
	"fmt"
	"os"
	"reflect"
)

// setHardlinkAnchors records only links shared by paths found inside this
// workspace. The caller sorts files first, making the anchor deterministic.
func setHardlinkAnchors(files []scannedFile) error {
	groups := make([][]int, 0)
	byIdentity := make(map[string]int)
	for index, file := range files {
		identity, hasIdentity := fileIdentity(file.info)
		group := -1
		if hasIdentity {
			if existing, ok := byIdentity[identity]; ok {
				if !os.SameFile(files[groups[existing][0]].info, file.info) {
					return fmt.Errorf("file %q changed during scan: %w", file.file.Path, ErrRescanRequired)
				}
				group = existing
			}
		} else {
			for candidate, members := range groups {
				if os.SameFile(files[members[0]].info, file.info) {
					group = candidate
					break
				}
			}
		}
		if group < 0 {
			group = len(groups)
			groups = append(groups, []int{index})
			if hasIdentity {
				byIdentity[identity] = group
			}
			continue
		}
		groups[group] = append(groups[group], index)
	}
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		anchor := files[members[0]].file.Path
		for _, index := range members[1:] {
			files[index].file.HardlinkTo = anchor
		}
	}
	return nil
}

func fileIdentity(info os.FileInfo) (string, bool) {
	value := statValue(info)
	if !value.IsValid() {
		return "", false
	}
	// Unix Stat_t values use Dev/Ino. Windows' os.fileStat stores the same
	// identity as vol plus a 64-bit file index.
	if dev, ok := numericField(value.FieldByName("Dev")); ok {
		if ino, ok := numericField(value.FieldByName("Ino")); ok {
			return fmt.Sprintf("%T:%s:%s", info.Sys(), dev, ino), true
		}
	}
	if vol, ok := numericField(value.FieldByName("vol")); ok {
		if high, ok := numericField(value.FieldByName("idxhi")); ok {
			if low, ok := numericField(value.FieldByName("idxlo")); ok {
				return fmt.Sprintf("%T:%s:%s:%s", info.Sys(), vol, high, low), true
			}
		}
	}
	return "", false
}

func statValue(info os.FileInfo) reflect.Value {
	if info == nil {
		return reflect.Value{}
	}
	value := reflect.ValueOf(info.Sys())
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	return value
}

func numericField(field reflect.Value) (string, bool) {
	if !field.IsValid() {
		return "", false
	}
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return fmt.Sprintf("%d", field.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return fmt.Sprintf("%d", field.Uint()), true
	default:
		return "", false
	}
}
