package runtimebrowser

import (
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	maxBrowserURLBytes           = 8192
	maxBrowserSelectorBytes      = 1024
	maxBrowserObservedValueBytes = 64 << 10
	maxBrowserTextBytes          = 4096
	maxBrowserNameBytes          = 512
	maxSnapshotTextBytes         = 64 << 10
	maxScreenshotBytes           = 512 << 10
)

func validOperationID(value string) bool { return validShortID(value, 'o') }
func validTaskID(value string) bool      { return validShortID(value, 'q') }

func validShortID(value string, prefix byte) bool {
	if len(value) != 8 || value[0] != prefix {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !(value[index] >= 'a' && value[index] <= 'z') && !(value[index] >= '0' && value[index] <= '9') {
			return false
		}
	}
	return true
}

func validContextID(value string) bool { return validOpaqueID(value, 256) }
func validTargetID(value string) bool  { return validOpaqueID(value, 256) }

func validOpaqueID(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func validBrowserAction(value string) bool {
	switch value {
	case "open_tab", "navigate", "handoff", "snapshot", "field", "click", "type", "press_key", "screenshot":
		return true
	default:
		return false
	}
}

func validCSSSelector(value string) bool {
	if len(value) == 0 || len(value) > maxBrowserSelectorBytes || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func validMarkerURL(value string) bool {
	return strings.HasPrefix(value, "about:blank#morde-") && len(value) <= 128
}

func cleanURL(raw string) (string, error) {
	if len(raw) == 0 || len(raw) > maxBrowserURLBytes || strings.TrimSpace(raw) != raw || !utf8.ValidString(raw) {
		return "", errors.New("browser URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
		return "", errors.New("browser URL is invalid")
	}
	for _, char := range raw {
		if char < 0x20 || char == 0x7f {
			return "", errors.New("browser URL is invalid")
		}
	}
	return parsed.String(), nil
}

func validAccessibleRole(value string) bool {
	switch value {
	case "button", "link", "textbox", "searchbox", "checkbox", "radio", "combobox", "listbox", "option", "menuitem", "tab", "switch":
		return true
	default:
		return false
	}
}

func validEditableRole(value string) bool {
	switch value {
	case "textbox", "searchbox", "combobox":
		return true
	default:
		return false
	}
}

func validKey(value string) bool {
	switch value {
	case "Enter", "Tab", "Escape", "Backspace", "Delete", "ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight", "Home", "End", "PageUp", "PageDown", "Space":
		return true
	default:
		return false
	}
}
