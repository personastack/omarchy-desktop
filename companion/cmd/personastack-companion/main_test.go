package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/personastack/omarchy-desktop/companion/internal/desktopbridge"
	"github.com/personastack/omarchy-desktop/companion/internal/installation"
	"github.com/personastack/personastack-api/pkg/client/apicontract"
)

func TestServeProcessesBoundedRequestResponseLines(t *testing.T) {
	t.Parallel()
	service := &serviceFake{status: installation.Status{Enrolled: true, CredentialValid: true}}
	processor, err := desktopbridge.New(service, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	input := strings.NewReader("{\"id\":1,\"action\":\"status\"}\n{\"id\":2,\"action\":\"revoke\"}\n")
	var output bytes.Buffer
	if err := serve(input, &output, processor); err != nil {
		t.Fatalf("serve() error = %v", err)
	}
	var responses []map[string]any
	decoder := json.NewDecoder(&output)
	for range 2 {
		var response map[string]any
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err)
		}
		responses = append(responses, response)
	}
	if len(responses) != 2 || responses[0]["id"] != float64(1) || responses[1]["id"] != float64(2) || responses[1]["ok"] != true {
		t.Fatalf("responses = %#v", responses)
	}
	if !reflect.DeepEqual(service.calls, []string{"status", "revoke"}) {
		t.Fatalf("service calls = %v", service.calls)
	}
}

func TestServeStopsOnMalformedOrOversizedRequest(t *testing.T) {
	t.Parallel()
	processor, err := desktopbridge.New(&serviceFake{}, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		"{\"id\":1,\"action\":\"revoke\",\"action\":\"status\"}\n",
		"{\"id\":1,\"action\":\"status\",\"padding\":\"" + strings.Repeat("x", desktopbridge.MaxRequestBytes) + "\"}\n",
	} {
		var output bytes.Buffer
		if err := serve(strings.NewReader(input), &output, processor); err == nil {
			t.Fatal("serve() accepted malformed IPC input")
		}
		if output.Len() != 0 {
			t.Fatalf("malformed IPC request produced output %q", output.String())
		}
	}
}

func TestServeRequiresProcessorAndPropagatesOutputFailure(t *testing.T) {
	t.Parallel()
	if err := serve(strings.NewReader(""), &bytes.Buffer{}, nil); err == nil {
		t.Fatal("serve() accepted a nil processor")
	}
	processor, err := desktopbridge.New(&serviceFake{}, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	if err := serve(strings.NewReader("{\"id\":1,\"action\":\"status\"}\n"), errorWriter{}, processor); err == nil {
		t.Fatal("serve() swallowed writer error")
	}
}

type serviceFake struct {
	calls  []string
	status installation.Status
}

func (s *serviceFake) Enroll(_ context.Context, _ string, _ apicontract.DesktopControlOperatingSystem) error {
	s.calls = append(s.calls, "enroll")
	return nil
}

func (s *serviceFake) Attach(_ context.Context, _, _ string) error {
	s.calls = append(s.calls, "attach")
	return nil
}

func (s *serviceFake) Status(_ context.Context, _ string) (installation.Status, error) {
	s.calls = append(s.calls, "status")
	return s.status, nil
}

func (s *serviceFake) Revoke(_ context.Context, _ string) error {
	s.calls = append(s.calls, "revoke")
	return nil
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}
