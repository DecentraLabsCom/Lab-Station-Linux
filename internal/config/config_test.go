package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "station.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultsAreSafeAndPortable(t *testing.T) {
	cfg := Defaults()
	if cfg.Profile != "dedicated" || cfg.ManagementUser != "labstation-ops" || cfg.Transport != "ssh" {
		t.Fatalf("unsafe or unexpected defaults: %#v", cfg)
	}
	if cfg.StateDir != "/var/lib/decentralabs/lab-station/data" || cfg.ConfigDir != "/etc/decentralabs/lab-station" {
		t.Fatalf("unexpected FHS paths: %#v", cfg)
	}
	if cfg.GuardGraceSeconds != 90 || cfg.Application.CloseTimeoutSeconds != 10 {
		t.Fatalf("unexpected session defaults: %#v", cfg)
	}
}
func TestLoadPreservesQuotedCommentsAndCommas(t *testing.T) {
	path := writeConfig(t, `[station]
name = "Engineering #1" # comments only start outside a string
profile = "hybrid"
guard_grace_seconds = 45
allow_local_session_eviction = true

[application]
command = "/opt/lab/apps/controller"
args = ["--label=course,#1", "quote \\\"ok\\\"", "--color=blue,green"]
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "Engineering #1" || cfg.Profile != "hybrid" || !cfg.AllowLocalSessionEviction {
		t.Fatalf("station settings were not parsed correctly: %#v", cfg)
	}
	want := []string{"--label=course,#1", `quote \"ok\"`, "--color=blue,green"}
	if len(cfg.Application.Args) != len(want) {
		t.Fatalf("args = %#v, want %#v", cfg.Application.Args, want)
	}
	for i := range want {
		if cfg.Application.Args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, cfg.Application.Args[i], want[i])
		}
	}
}

func TestLoadApplicationCloseCoordinatesForSingleAndPrimaryMultiApp(t *testing.T) {
	single, err := Load(writeConfig(t, `[application]
command = "/opt/lab/apps/controller"
close_coordinates = [394, 395]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(single.Application.CloseCoordinates) != 2 || single.Application.CloseCoordinates[0] != 394 || single.Application.CloseCoordinates[1] != 395 {
		t.Fatalf("single-app close coordinates = %#v", single.Application.CloseCoordinates)
	}

	multi, err := Load(writeConfig(t, `[application]
command = ""

[[application.apps]]
id = "primary"
command = "/opt/lab/apps/primary"
close_coordinates = [120, 240]

[[application.apps]]
id = "secondary"
command = "/opt/lab/apps/secondary"
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(multi.Applications) != 2 || len(multi.Applications[0].CloseCoordinates) != 2 || multi.Applications[0].CloseCoordinates[0] != 120 || len(multi.Applications[1].CloseCoordinates) != 0 {
		t.Fatalf("multi-app close coordinates = %#v", multi.Applications)
	}
}

func TestLoadRejectsMalformedAndUnknownSettings(t *testing.T) {
	cases := map[string]string{
		"missing equals":        "[station]\nprofile \"hybrid\"\n",
		"unknown section":       "[other]\nname = \"x\"\n",
		"unknown setting":       "[station]\nroot_command = \"bash\"\n",
		"wrong type":            "[station]\nmanagement_port = \"22\"\n",
		"invalid string escape": "[station]\nname = \"bad\\q\"\n",
		"invalid array item":    "[application]\nargs = [22]\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, content)); err == nil {
				t.Fatal("Load accepted malformed or unknown configuration")
			}
		})
	}
}

func TestLoadRejectsUnsafeAndOutOfRangeSettings(t *testing.T) {
	cases := map[string]string{
		"unsupported profile":                "[station]\nprofile = \"desktop\"\n",
		"privileged ssh user":                "[station]\nmanagement_user = \"root\"\n",
		"invalid ssh port low":               "[station]\nmanagement_port = 0\n",
		"invalid ssh port high":              "[station]\nmanagement_port = 65536\n",
		"state path traversal":               "[station]\nstate_dir = \"/var/lib/decentralabs/lab-station/../../etc\"\n",
		"external state path":                "[station]\nstate_dir = \"/tmp/lab\"\n",
		"alternate config path":              "[station]\nconfig_dir = \"/tmp\"\n",
		"relative log path":                  "[station]\nlog_dir = \"logs\"\n",
		"shell command":                      "[application]\ncommand = \"/bin/sh\"\n",
		"shell metacharacter argument":       "[application]\nargs = [\"--run=$(touch /tmp/pwned)\"]\n",
		"newline argument":                   "[application]\nargs = [\"line\\nnext\"]\n",
		"guard below zero":                   "[station]\nguard_grace_seconds = -1\n",
		"guard above limit":                  "[station]\nguard_grace_seconds = 91\n",
		"application timeout zero":           "[application]\nclose_timeout_seconds = 0\n",
		"application timeout too high":       "[application]\nclose_timeout_seconds = 301\n",
		"close coordinates missing y":        "[application]\ncommand = \"/opt/lab/apps/demo\"\nclose_coordinates = [12]\n",
		"negative close coordinate":          "[application]\ncommand = \"/opt/lab/apps/demo\"\nclose_coordinates = [-1, 20]\n",
		"oversized close coordinate":         "[application]\ncommand = \"/opt/lab/apps/demo\"\nclose_coordinates = [20, 32768]\n",
		"multi-app coordinates on secondary": "[application]\ncommand = \"\"\n[[application.apps]]\nid = \"primary\"\ncommand = \"/opt/lab/apps/primary\"\n[[application.apps]]\nid = \"secondary\"\ncommand = \"/opt/lab/apps/secondary\"\nclose_coordinates = [20, 30]\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, content)); err == nil {
				t.Fatal("Load accepted unsafe or out-of-range configuration")
			}
		})
	}
}

func TestLoadRejectsMoreThanThirtyTwoApplicationArguments(t *testing.T) {
	args := make([]string, 33)
	for i := range args {
		args[i] = `"value"`
	}
	content := "[application]\nargs = [" + strings.Join(args, ",") + "]\n"
	if _, err := Load(writeConfig(t, content)); err == nil {
		t.Fatal("Load accepted more than 32 application arguments")
	}
}
