package gateway

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

var errInvalidHost = errors.New("invalid host")

// ParsedHost is the host label carried by the gateway request authority.
type ParsedHost struct {
	HostID      string
	PreviewPort *int
}

func parseHost(authority, hostSuffix string) (ParsedHost, error) {
	host, err := stripAuthorityPort(authority)
	if err != nil {
		return ParsedHost{}, errInvalidHost
	}

	host = strings.ToLower(host)
	hostSuffix = strings.ToLower(hostSuffix)
	if hostSuffix == "" || !strings.HasSuffix(host, "."+hostSuffix) {
		return ParsedHost{}, errInvalidHost
	}
	label := strings.TrimSuffix(host, "."+hostSuffix)
	if label == "" || strings.Contains(label, ".") {
		return ParsedHost{}, errInvalidHost
	}

	parsed := ParsedHost{}
	if portText, hostID, found := strings.Cut(label, "-"); found {
		if len(portText) == 0 || len(portText) > 5 || (len(portText) > 1 && portText[0] == '0') {
			return ParsedHost{}, errInvalidHost
		}
		for _, digit := range portText {
			if digit < '0' || digit > '9' {
				return ParsedHost{}, errInvalidHost
			}
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return ParsedHost{}, errInvalidHost
		}
		parsed.PreviewPort = &port
		label = hostID
	}

	if len(label) != 8 || label[0] != 'h' {
		return ParsedHost{}, errInvalidHost
	}
	for _, char := range label[1:] {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return ParsedHost{}, errInvalidHost
		}
	}
	parsed.HostID = label
	return parsed, nil
}

func stripAuthorityPort(authority string) (string, error) {
	if !strings.Contains(authority, ":") {
		return authority, nil
	}
	host, portText, err := net.SplitHostPort(authority)
	if err != nil || host == "" || portText == "" {
		return "", errInvalidHost
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", errInvalidHost
	}
	return host, nil
}
