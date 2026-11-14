package host

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
)

var xrdpConfigPath = "/etc/xrdp/xrdp.ini"
var procTCPTablePaths = []string{"/proc/net/tcp", "/proc/net/tcp6"}

// RDPClientsConnected reports whether an established TCP socket is attached
// to one of the configured XRDP listener ports. Unknown configuration or
// socket state is reported separately so callers can fail closed.
func (LinuxRuntime) RDPClientsConnected() (bool, bool) {
	config, err := os.ReadFile(xrdpConfigPath)
	if err != nil {
		return false, false
	}
	ports, err := parseXRDPListenPorts(config)
	if err != nil {
		return false, false
	}
	portSet := make(map[uint16]struct{}, len(ports))
	for _, port := range ports {
		portSet[port] = struct{}{}
	}
	for index, path := range procTCPTablePaths {
		content, err := os.ReadFile(path)
		if err != nil {
			// Kernels without IPv6 may not expose tcp6. In that case the IPv4
			// socket table still provides a complete view of possible clients.
			if index == 1 && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, false
		}
		connected, err := procTCPHasEstablishedConnection(content, portSet)
		if err != nil {
			return false, false
		}
		if connected {
			return true, true
		}
	}
	return false, true
}

func parseXRDPListenPorts(config []byte) ([]uint16, error) {
	section := ""
	foundGlobals := false
	ports := map[uint16]struct{}{}
	scanner := bufio.NewScanner(strings.NewReader(string(config)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			if section == "globals" {
				foundGlobals = true
			}
			continue
		}
		if section != "globals" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "port") {
			continue
		}
		port, err := parseXRDPPort(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("invalid XRDP TCP listener: %w", err)
		}
		ports[port] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !foundGlobals {
		return nil, errors.New("XRDP [Globals] section is missing")
	}
	if len(ports) == 0 {
		ports[3389] = struct{}{}
	}
	result := make([]uint16, 0, len(ports))
	for port := range ports {
		result = append(result, port)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func parseXRDPPort(value string) (uint16, error) {
	portText := value
	if strings.Contains(value, "://") {
		scheme, endpoint, _ := strings.Cut(value, "://")
		if !strings.EqualFold(scheme, "tcp") && !strings.EqualFold(scheme, "ssl") {
			return 0, fmt.Errorf("unsupported transport %q", scheme)
		}
		_, parsedPort, err := net.SplitHostPort(endpoint)
		if err != nil {
			return 0, fmt.Errorf("invalid listener address %q", value)
		}
		portText = parsedPort
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return 0, fmt.Errorf("invalid port %q", portText)
	}
	return uint16(port), nil
}

func procTCPHasEstablishedConnection(content []byte, ports map[uint16]struct{}) (bool, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	if !scanner.Scan() {
		return false, errors.New("TCP socket inventory is empty")
	}
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 {
			return false, errors.New("TCP socket inventory contains a malformed entry")
		}
		localAddress := fields[1]
		separator := strings.LastIndexByte(localAddress, ':')
		if separator < 0 || len(localAddress[separator+1:]) != 4 {
			return false, errors.New("TCP socket inventory contains an invalid local address")
		}
		portValue, err := strconv.ParseUint(localAddress[separator+1:], 16, 16)
		if err != nil {
			return false, errors.New("TCP socket inventory contains an invalid local port")
		}
		if fields[3] != "01" {
			continue
		}
		if _, found := ports[uint16(portValue)]; found {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, nil
}
