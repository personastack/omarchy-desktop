//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestServeAsyncOutputFailureUnblocksOpenInput(t *testing.T) {
	t.Parallel()
	service := &serviceFake{}
	processor, err := desktopbridge.New(service, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	inputReader, inputWriter := io.Pipe()
	serveDone := make(chan error, 1)
	output := &failAfterWrites{remaining: 1}
	go func() { serveDone <- serve(inputReader, output, processor) }()
	for _, line := range []string{`{"id":1,"version":"1","action":"sync","scope":"workspace:a"}`, `{"id":2,"version":"1","action":"state","scope":"workspace:a"}`} {
		if _, err := io.WriteString(inputWriter, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-serveDone:
		if err == nil {
			t.Fatal("serve() swallowed asynchronous output error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve() remained blocked on open input after output failure")
	}
	_ = inputWriter.Close()
}

func TestServeReadsScopeSyncWhileARequestIsInFlight(t *testing.T) {
	t.Parallel()
	service := &blockingStateService{started: make(chan struct{})}
	processor, err := desktopbridge.New(service, "https://my.personastack.ai")
	if err != nil {
		t.Fatal(err)
	}
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(inputReader, outputWriter, processor) }()
	responses := make(chan desktopbridge.Response, 3)
	go func() {
		defer close(responses)
		scanner := bufio.NewScanner(outputReader)
		for scanner.Scan() {
			var response desktopbridge.Response
			if json.Unmarshal(scanner.Bytes(), &response) == nil {
				responses <- response
			}
		}
	}()
	for _, line := range []string{
		`{"id":1,"version":"1","action":"sync","scope":"workspace:a"}`,
		`{"id":2,"version":"1","action":"state","scope":"workspace:a"}`,
	} {
		if _, err := io.WriteString(inputWriter, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	<-service.started
	if _, err := io.WriteString(inputWriter, `{"id":3,"version":"1","action":"sync","scope":"workspace:b"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatal(err)
	}
	seen := make(map[uint64]desktopbridge.Response, 3)
	for range 3 {
		response := <-responses
		seen[response.ID] = response
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
	_ = outputWriter.Close()
	if len(seen) != 3 || !seen[1].OK || !seen[3].OK || seen[2].Error != desktopbridge.ErrorStaleRequest {
		t.Fatalf("responses after scope change = %#v", seen)
	}
}

type serviceFake struct {
	calls []string
	state installation.LocalState
}

type blockingStateService struct {
	started chan struct{}
}

func (s *blockingStateService) LocalState(ctx context.Context, _ string) (installation.LocalState, error) {
	close(s.started)
	<-ctx.Done()
	return installation.LocalState{}, ctx.Err()
}

func (s *serviceFake) LocalState(_ context.Context, _ string) (installation.LocalState, error) {
	s.calls = append(s.calls, "state")
	return s.state, nil
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

type failAfterWrites struct {
	remaining int
}

func (w *failAfterWrites) Write(data []byte) (int, error) {
	if w.remaining == 0 {
		return 0, errors.New("write failed")
	}
	w.remaining--
	return len(data), nil
}
