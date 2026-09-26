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
)

func TestServeProcessesBoundedRequestResponseLines(t *testing.T) {
	t.Parallel()
	service := &serviceFake{state: installation.LocalState{RelayPaused: true}}
	processor, err := desktopbridge.New(service, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	input := strings.NewReader("{\"id\":1,\"version\":\"1\",\"action\":\"sync\",\"scope\":\"\"}\n{\"id\":2,\"version\":\"1\",\"action\":\"state\",\"scope\":\"\"}\n")
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
	if !reflect.DeepEqual(service.calls, []string{"state"}) {
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
		"{\"id\":1,\"version\":\"1\",\"action\":\"sync\",\"action\":\"state\",\"scope\":\"\"}\n",
		"{\"id\":1,\"version\":\"1\",\"action\":\"sync\",\"scope\":\"" + strings.Repeat("x", desktopbridge.MaxRequestBytes) + "\"}\n",
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
	if err := serve(strings.NewReader("{\"id\":1,\"version\":\"1\",\"action\":\"sync\",\"scope\":\"\"}\n"), errorWriter{}, processor); err == nil {
		t.Fatal("serve() swallowed writer error")
	}
}

type serviceFake struct {
	calls  []string
	state  installation.LocalState
}

func (s *serviceFake) LocalState(_ context.Context, _ string) (installation.LocalState, error) {
	s.calls = append(s.calls, "state")
	return s.state, nil
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}
