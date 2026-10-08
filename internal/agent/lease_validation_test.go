package agent

import (
	"testing"
	"time"
)

func validV2LeaseRequest(now time.Time, command string) Request {
	return Request{
		SchemaVersion: 2,
		ID:            "operation-1",
		Operation:     "execute",
		Command:       command,
		IssuedAt:      now.Format(time.RFC3339Nano),
		ExecuteBefore: now.Add(time.Minute).Format(time.RFC3339Nano),
		Context: &LeaseContext{
			Kind:           "reservation",
			LabID:          "lab-1",
			ReservationKey: "reservation-1",
			LeaseID:        "lease-1",
			NotBefore:      now.Format(time.RFC3339Nano),
			ExpiresAt:      now.Add(time.Hour).Format(time.RFC3339Nano),
		},
	}
}

func TestValidateLeaseRequestAcceptsReservationAndDemoLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	prepare := validV2LeaseRequest(now, "prepare-session")
	if err := ValidateLeaseRequest(prepare, now); err != nil {
		t.Fatalf("valid reservation prepare was rejected: %v", err)
	}

	release := validV2LeaseRequest(now, "release-session")
	release.Context.Generation = 1
	if err := ValidateLeaseRequest(release, now); err != nil {
		t.Fatalf("valid reservation release was rejected: %v", err)
	}

	demo := validV2LeaseRequest(now, "prepare-session")
	demo.Context.Kind = "demo"
	demo.Context.LabID = ""
	demo.Context.ReservationKey = ""
	demo.Context.LeaseID = "demo:lease-1"
	if err := ValidateLeaseRequest(demo, now); err != nil {
		t.Fatalf("valid demo prepare was rejected: %v", err)
	}
}

func TestValidateLeaseRequestRejectsInvalidLifecycleInputs(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		change func(*Request)
	}{
		{"missing context", func(r *Request) { r.Context = nil }},
		{"wrong operation", func(r *Request) { r.Operation = "status" }},
		{"wrong command", func(r *Request) { r.Command = "power shutdown" }},
		{"invalid issuedAt", func(r *Request) { r.IssuedAt = "yesterday" }},
		{"invalid execution deadline", func(r *Request) { r.ExecuteBefore = "yesterday" }},
		{"execution deadline before issue", func(r *Request) { r.ExecuteBefore = r.IssuedAt }},
		{"execution window too long", func(r *Request) { r.ExecuteBefore = now.Add(6 * time.Minute).Format(time.RFC3339Nano) }},
		{"future issue time", func(r *Request) { r.IssuedAt = now.Add(time.Second).Format(time.RFC3339Nano) }},
		{"expired execution window", func(r *Request) { r.ExecuteBefore = now.Add(-time.Second).Format(time.RFC3339Nano) }},
		{"non UTC issue time", func(r *Request) { r.IssuedAt = now.In(time.FixedZone("plus one", 3600)).Format(time.RFC3339Nano) }},
		{"invalid lease kind", func(r *Request) { r.Context.Kind = "other" }},
		{"invalid lease id", func(r *Request) { r.Context.LeaseID = "bad id" }},
		{"reservation missing lab id", func(r *Request) { r.Context.LabID = "" }},
		{"reservation missing key", func(r *Request) { r.Context.ReservationKey = "" }},
		{"demo without demo prefix", func(r *Request) { r.Context.Kind = "demo" }},
		{"invalid notBefore", func(r *Request) { r.Context.NotBefore = "later" }},
		{"invalid expiry", func(r *Request) { r.Context.ExpiresAt = "later" }},
		{"expiry before notBefore", func(r *Request) { r.Context.ExpiresAt = now.Format(time.RFC3339Nano) }},
		{"prepare generation set", func(r *Request) { r.Context.Generation = 1 }},
		{"prepare already expired", func(r *Request) { r.Context.ExpiresAt = now.Format(time.RFC3339Nano) }},
		{"release generation missing", func(r *Request) { r.Command = "release-session" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validV2LeaseRequest(now, "prepare-session")
			test.change(&request)
			if err := ValidateLeaseRequest(request, now); err == nil {
				t.Fatal("invalid lease request was accepted")
			}
		})
	}
}

func TestIsReadOnlyV2CommandRequiresExactAllowlistedArguments(t *testing.T) {
	tests := []struct {
		command string
		args    []string
		want    bool
	}{
		{"identity", nil, true},
		{"status-json", nil, true},
		{"energy audit", nil, true},
		{"service", []string{"status"}, true},
		{"fmu-executor", []string{"status"}, true},
		{"local-mode", []string{"status"}, true},
		{"service", []string{"status", "--verbose"}, false},
		{"prepare-session", nil, false},
	}
	for _, test := range tests {
		if got := isReadOnlyV2Command(test.command, test.args); got != test.want {
			t.Errorf("isReadOnlyV2Command(%q, %v) = %v, want %v", test.command, test.args, got, test.want)
		}
	}
}

func TestCheckLeaseAdmissionRejectsPathsOutsideManagedRoot(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, path := range []string{"relative/state", "/tmp/station", "/var/lib/decentralabs/lab-station/../other"} {
		if err := CheckLeaseAdmission(path, now); err == nil {
			t.Errorf("CheckLeaseAdmission(%q) accepted a path outside the managed state root", path)
		}
	}
}
