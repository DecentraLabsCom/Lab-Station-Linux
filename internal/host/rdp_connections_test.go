package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestXRDPListenPortsParseConfiguredTCPListeners(t *testing.T) {
	tests := []struct {
		name string
		ini  string
		want []uint16
	}{
		{name: "default port", ini: "[Globals]\nallow_channels=false\n", want: []uint16{3389}},
		{name: "plain port", ini: "[Globals]\nport=3390\n", want: []uint16{3390}},
		{name: "tcp uri", ini: "[Globals]\nport=tcp://:3391\n", want: []uint16{3391}},
		{name: "ssl uri", ini: "[Globals]\nport=ssl://127.0.0.1:3392\n", want: []uint16{3392}},
		{name: "comment ignored", ini: "[Globals]\n;port=3393\n#port=3394\n", want: []uint16{3389}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseXRDPListenPorts([]byte(test.ini))
			if err != nil {
				t.Fatalf("parseXRDPListenPorts() error = %v", err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("ports = %v, want %v", got, test.want)
			}
			for index := range got {
				if got[index] != test.want[index] {
					t.Fatalf("ports = %v, want %v", got, test.want)
				}
			}
		})
	}
}

func TestXRDPListenPortsRejectAmbiguousOrInvalidConfiguration(t *testing.T) {
	for _, ini := range []string{
		"[Globals]\nport=ask\n",
		"[Globals]\nport=tcp://:0\n",
		"[Globals]\nport=vsock://2:3389\n",
		"[Other]\nport=3390\n",
	} {
		if ports, err := parseXRDPListenPorts([]byte(ini)); err == nil {
			t.Errorf("parseXRDPListenPorts(%q) = %v, nil error; want fail-closed error", ini, ports)
		}
	}
}

func TestProcTCPHasEstablishedConnectionOnlyOnConfiguredRDPPort(t *testing.T) {
	proc := `  sl  local_address rem_address   st
   0: 0100007F:0D3D 0100007F:CE01 01 00000000:00000000 00:00000000 00000000 1000 0 12345
   1: 00000000:0D3D 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 12346
   2: 0100007F:0D3E 0100007F:CE02 01 00000000:00000000 00:00000000 00000000 1000 0 12347
`
	connected, err := procTCPHasEstablishedConnection([]byte(proc), map[uint16]struct{}{3389: {}})
	if err != nil || !connected {
		t.Fatalf("established RDP socket not detected: connected=%v err=%v", connected, err)
	}
	connected, err = procTCPHasEstablishedConnection([]byte(proc), map[uint16]struct{}{3391: {}})
	if err != nil || connected {
		t.Fatalf("connection on another port was treated as RDP: connected=%v err=%v", connected, err)
	}
	if _, err := procTCPHasEstablishedConnection([]byte(strings.TrimSpace("")), map[uint16]struct{}{3389: {}}); err == nil {
		t.Fatal("malformed proc TCP inventory was not rejected")
	}
}

func TestRDPClientsConnectedUsesIPv4WhenKernelHasNoIPv6SocketTable(t *testing.T) {
	temp := t.TempDir()
	configPath := filepath.Join(temp, "xrdp.ini")
	tcpPath := filepath.Join(temp, "tcp")
	if err := os.WriteFile(configPath, []byte("[Globals]\nport=3389\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tcpPath, []byte("sl local_address rem_address st\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldConfigPath, oldTablePaths := xrdpConfigPath, procTCPTablePaths
	xrdpConfigPath = configPath
	procTCPTablePaths = []string{tcpPath, filepath.Join(temp, "tcp6-missing")}
	t.Cleanup(func() {
		xrdpConfigPath = oldConfigPath
		procTCPTablePaths = oldTablePaths
	})

	connected, known := (LinuxRuntime{}).RDPClientsConnected()
	if connected || !known {
		t.Fatalf("IPv4-only RDP state = (%v, %v), want (false, true)", connected, known)
	}
}
