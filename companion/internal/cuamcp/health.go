package cuamcp

import (
	"encoding/json"
	"errors"
	"strings"
)

var ErrInvalidHealthReport = errors.New("Cua health report is invalid or unsupported")

type HealthReportSnapshot struct {
	SchemaVersion string        `json:"schema_version"`
	Platform      string        `json:"platform"`
	DriverVersion string        `json:"driver_version"`
	Overall       string        `json:"overall"`
	Checks        []HealthCheck `json:"checks"`
}

type HealthCheck struct {
	Name    string          `json:"name"`
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Hint    string          `json:"hint,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

var requiredLinuxHealthChecks = []string{
	"binary_version", "platform_supported", "session_active", "ax_capability", "screen_capture_capability",
}

func (r HealthReportSnapshot) ReadyFor(expectedDriverVersion string) bool {
	if strings.TrimSpace(expectedDriverVersion) == "" || !validHealthReport(r) ||
		r.DriverVersion != expectedDriverVersion || r.Overall != "ok" {
		return false
	}
	statuses := make(map[string]string, len(r.Checks))
	for _, check := range r.Checks {
		statuses[check.Name] = check.Status
	}
	for _, required := range requiredLinuxHealthChecks {
		if statuses[required] != "pass" {
			return false
		}
	}
	return true
}

func parseHealthReport(raw json.RawMessage) (HealthReportSnapshot, error) {
	var result toolCallResult
	if !validObject(raw) || json.Unmarshal(raw, &result) != nil || !validObject(result.StructuredContent) {
		return HealthReportSnapshot{}, ErrInvalidHealthReport
	}
	var report HealthReportSnapshot
	if json.Unmarshal(result.StructuredContent, &report) != nil || !validHealthReport(report) {
		return HealthReportSnapshot{}, ErrInvalidHealthReport
	}
	return report, nil
}

func validHealthReport(report HealthReportSnapshot) bool {
	if report.SchemaVersion != "1" || report.Platform != "linux" || strings.TrimSpace(report.DriverVersion) == "" ||
		(report.Overall != "ok" && report.Overall != "degraded" && report.Overall != "failed") || len(report.Checks) == 0 {
		return false
	}
	seen := make(map[string]string, len(report.Checks))
	coreFailure := false
	anyFailure := false
	for _, check := range report.Checks {
		if strings.TrimSpace(check.Name) == "" || strings.TrimSpace(check.Message) == "" ||
			(check.Status != "pass" && check.Status != "fail" && check.Status != "skip") {
			return false
		}
		if _, exists := seen[check.Name]; exists {
			return false
		}
		seen[check.Name] = check.Status
		if len(check.Data) > 0 && !validObject(check.Data) {
			return false
		}
		if check.Status == "fail" {
			anyFailure = true
			if coreHealthCheck(check.Name) {
				coreFailure = true
			}
		}
	}
	for _, required := range requiredLinuxHealthChecks {
		if _, exists := seen[required]; !exists {
			return false
		}
	}
	for _, required := range []string{"binary_version", "platform_supported", "session_active"} {
		if seen[required] == "skip" {
			return false
		}
	}
	wantOverall := "ok"
	if coreFailure {
		wantOverall = "failed"
	} else if anyFailure {
		wantOverall = "degraded"
	}
	return report.Overall == wantOverall
}

func coreHealthCheck(name string) bool {
	switch name {
	case "binary_version", "platform_supported", "session_active":
		return true
	default:
		return false
	}
}
