package cuamcp

import (
	"encoding/json"
	"testing"
)

func TestHealthReportRequiresAllLinuxChecksToBeReady(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*HealthReportSnapshot)
		ready  bool
	}{
		{name: "all required checks pass", ready: true},
		{name: "non-core capability fails", change: func(report *HealthReportSnapshot) {
			report.Overall = "degraded"
			report.Checks[3].Status = "fail"
		}, ready: false},
		{name: "core session check fails", change: func(report *HealthReportSnapshot) {
			report.Overall = "failed"
			report.Checks[2].Status = "fail"
		}, ready: false},
		{name: "unknown failed check degrades readiness", change: func(report *HealthReportSnapshot) {
			report.Overall = "degraded"
			report.Checks = append(report.Checks, HealthCheck{Name: "future_check", Status: "fail", Message: "not ready"})
		}, ready: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			report := validHealthReportFixture()
			if tc.change != nil {
				tc.change(&report)
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			raw := healthEnvelope(encoded)
			got, err := parseHealthReport(raw)
			if err != nil || got.ReadyFor("0.30.1") != tc.ready {
				t.Fatalf("health report = %#v, ready=%v, error=%v", got, got.ReadyFor("0.30.1"), err)
			}
		})
	}
}

func TestHealthReportReadinessRequiresSelectedDriverVersion(t *testing.T) {
	t.Parallel()
	report := validHealthReportFixture()
	if report.ReadyFor("0.28.2") || report.ReadyFor("") {
		t.Fatal("health report passed readiness for a different or unspecified driver version")
	}
}

func TestHealthReportRejectsMalformedOrInconsistentContracts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*HealthReportSnapshot)
	}{
		{name: "unsupported schema", change: func(report *HealthReportSnapshot) { report.SchemaVersion = "2" }},
		{name: "wrong platform", change: func(report *HealthReportSnapshot) { report.Platform = "darwin" }},
		{name: "missing driver version", change: func(report *HealthReportSnapshot) { report.DriverVersion = " " }},
		{name: "malformed driver version", change: func(report *HealthReportSnapshot) { report.DriverVersion = "garbage" }},
		{name: "invalid semantic driver version", change: func(report *HealthReportSnapshot) { report.DriverVersion = "0.29" }},
		{name: "driver version with leading zero", change: func(report *HealthReportSnapshot) { report.DriverVersion = "0.029.1" }},
		{name: "missing required check", change: func(report *HealthReportSnapshot) { report.Checks = report.Checks[1:] }},
		{name: "duplicate check", change: func(report *HealthReportSnapshot) { report.Checks = append(report.Checks, report.Checks[0]) }},
		{name: "invalid check status", change: func(report *HealthReportSnapshot) { report.Checks[0].Status = "unknown" }},
		{name: "core check skipped", change: func(report *HealthReportSnapshot) { report.Checks[0].Status = "skip" }},
		{name: "wrong aggregate status", change: func(report *HealthReportSnapshot) { report.Checks[2].Status = "fail" }},
		{name: "non-object check data", change: func(report *HealthReportSnapshot) { report.Checks[0].Data = json.RawMessage(`[]`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			report := validHealthReportFixture()
			tc.change(&report)
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseHealthReport(healthEnvelope(encoded)); err != ErrInvalidHealthReport {
				t.Fatalf("malformed health report error = %v", err)
			}
		})
	}
}

func TestHealthReportRequiresStructuredContent(t *testing.T) {
	t.Parallel()
	if _, err := parseHealthReport(json.RawMessage(`{"content":[{"type":"text","text":"ready"}]}`)); err != ErrInvalidHealthReport {
		t.Fatalf("text-only health report error = %v", err)
	}
	if _, err := parseHealthReport(json.RawMessage(`{"structuredContent":{"schema_version":"1","schema_version":"1"}}`)); err != ErrInvalidHealthReport {
		t.Fatalf("duplicate health report field error = %v", err)
	}
}

func validHealthReportFixture() HealthReportSnapshot {
	checks := make([]HealthCheck, 0, len(requiredLinuxHealthChecks))
	for _, name := range requiredLinuxHealthChecks {
		checks = append(checks, HealthCheck{Name: name, Status: "pass", Message: "ready"})
	}
	return HealthReportSnapshot{SchemaVersion: "1", Platform: "linux", DriverVersion: "0.30.1", Overall: "ok", Checks: checks}
}

func healthEnvelope(structured json.RawMessage) json.RawMessage {
	result, _ := json.Marshal(toolCallResult{
		Content:           []json.RawMessage{json.RawMessage(`{"type":"text","text":"ready"}`)},
		StructuredContent: structured,
	})
	return result
}
