package template

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

var errInvalidQuantity = errors.New("invalid resource quantity")

func CPUMilli(value string) (int64, error) {
	if strings.HasSuffix(value, "m") {
		return positiveInt(strings.TrimSuffix(value, "m"))
	}
	cores, err := positiveInt(value)
	if err != nil || cores > math.MaxInt64/1000 {
		return 0, errInvalidQuantity
	}
	return cores * 1000, nil
}

func MemMiB(value string) (int64, error) {
	var factor int64
	switch {
	case strings.HasSuffix(value, "Mi"):
		factor = 1
		value = strings.TrimSuffix(value, "Mi")
	case strings.HasSuffix(value, "Gi"):
		factor = 1024
		value = strings.TrimSuffix(value, "Gi")
	case strings.HasSuffix(value, "Ki"):
		factor = 0
		value = strings.TrimSuffix(value, "Ki")
	default:
		return 0, errInvalidQuantity
	}
	n, err := positiveInt(value)
	if err != nil {
		return 0, err
	}
	if factor == 0 {
		if n%1024 != 0 {
			return 0, errInvalidQuantity
		}
		return n / 1024, nil
	}
	if n > math.MaxInt64/factor {
		return 0, errInvalidQuantity
	}
	return n * factor, nil
}

func positiveInt(value string) (int64, error) {
	if value == "" {
		return 0, errInvalidQuantity
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return 0, errInvalidQuantity
	}
	return n, nil
}
