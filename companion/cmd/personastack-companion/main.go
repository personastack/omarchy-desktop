//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/personastack/omarchy-desktop/companion/internal/credentialstore"
	"github.com/personastack/omarchy-desktop/companion/internal/cuaruntime"
	"github.com/personastack/omarchy-desktop/companion/internal/desktopbridge"
	"github.com/personastack/omarchy-desktop/companion/internal/desktoplifecycle"
	"github.com/personastack/omarchy-desktop/companion/internal/desktoplocal"
	"github.com/personastack/omarchy-desktop/companion/internal/hyprlandlock"
	"github.com/personastack/omarchy-desktop/companion/internal/installation"
	"github.com/personastack/omarchy-desktop/companion/localsession"
	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

const maxConcurrentBridgeRequests = 31

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	client, err := desktopcontrol.New(os.Args[1])
	if err != nil {
		os.Exit(2)
	}
	service, err := installation.New(client, credentialstore.NewSecretService())
	if err != nil {
		os.Exit(2)
	}
	managedRuntime, runtimeErr := cuaruntime.NewDefault()
	shell, shellErr := localsession.LoginShell()
	var controlRuntime *desktoplifecycle.Controller
	if runtimeErr == nil {
		if shellErr == nil {
			pausePreference, preferenceErr := desktoplifecycle.NewFilePausePreference()
			if preferenceErr != nil {
				err = preferenceErr
			} else {
				controlRuntime, err = desktoplifecycle.New(desktoplifecycle.Options{
					Origin: os.Args[1], Runtime: managedRuntime, Installations: service,
					LockProbe: hyprlandlock.NewProbe(), Local: desktoplocal.New(shell), PausePreference: pausePreference,
				})
			}
		}
		if shellErr != nil || err != nil {
			managedRuntime.Close()
		}
	}
	probe := localsession.NewProbe()
	installer, installerErr := localsession.DefaultFileInstaller()
	preferences, preferencesErr := localsession.NewFilePreferences()
	var processor *desktopbridge.Processor
	if controlRuntime != nil {
		var manager desktopbridge.LocalSessionService
		if installerErr == nil && preferencesErr == nil {
			if created, managerErr := localsession.NewManager(probe, installer, preferences); managerErr == nil {
				manager = created
			}
		}
		processor, err = desktopbridge.NewWithControlRuntime(service, manager, controlRuntime, os.Args[1])
	} else if installerErr == nil && preferencesErr == nil {
		manager, managerErr := localsession.NewManager(probe, installer, preferences)
		if managerErr == nil {
			processor, err = desktopbridge.NewWithLocalSessions(service, manager, os.Args[1])
		} else {
			processor, err = desktopbridge.New(service, os.Args[1])
		}
	} else {
		processor, err = desktopbridge.New(service, os.Args[1])
	}
	if err != nil {
		if controlRuntime != nil {
			closeControlRuntime(context.Background(), controlRuntime)
		}
		os.Exit(2)
	}
	if controlRuntime != nil {
		controlRuntime.StartRecovery()
	}
	serveErr := serve(os.Stdin, os.Stdout, processor)
	if controlRuntime != nil {
		closeControlRuntime(context.Background(), controlRuntime)
	}
	if serveErr != nil {
		os.Exit(1)
	}
}

func closeControlRuntime(ctx context.Context, runtime *desktoplifecycle.Controller) {
	for ctx.Err() == nil {
		closeContext, cancel := context.WithTimeout(ctx, 10*time.Second)
		closed := runtime.Close(closeContext)
		cancel()
		if closed {
			return
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func serve(input io.Reader, output io.Writer, processor *desktopbridge.Processor) error {
	if processor == nil {
		return errors.New("desktop bridge unavailable")
	}
	writer := bufio.NewWriter(output)
	processContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	inputDone := make(chan struct{})
	defer close(inputDone)
	type scannedRequest struct {
		line []byte
		err  error
		done bool
	}
	lines := make(chan scannedRequest)
	go func() {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 64*1024), desktopbridge.MaxRequestBytes)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- scannedRequest{line: line}:
			case <-inputDone:
				return
			}
		}
		select {
		case lines <- scannedRequest{err: scanner.Err(), done: true}:
		case <-inputDone:
		}
	}()
	requestSlots := make(chan struct{}, maxConcurrentBridgeRequests)
	var requests sync.WaitGroup
	var outputMu sync.Mutex
	outputErrors := make(chan error, 1)
	writeResponse := func(response desktopbridge.Response) error {
		outputMu.Lock()
		defer outputMu.Unlock()
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			return err
		}
		return writer.Flush()
	}
	shutdown := func() {
		cancel()
		processor.CancelAll()
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	for {
		var scanned scannedRequest
		select {
		case scanned = <-lines:
		case err := <-outputErrors:
			shutdown()
			requests.Wait()
			return err
		}
		if scanned.done {
			shutdown()
			requests.Wait()
			select {
			case err := <-outputErrors:
				return err
			default:
			}
			return scanned.err
		}
		request, err := desktopbridge.Parse(scanned.line)
		if err != nil {
			shutdown()
			requests.Wait()
			return err
		}
		if request.Action == desktopbridge.ActionSync {
			if err := writeResponse(processor.Handle(processContext, request)); err != nil {
				shutdown()
				requests.Wait()
				return err
			}
			continue
		}
		select {
		case requestSlots <- struct{}{}:
		default:
			response := desktopbridge.Response{ID: request.ID, Error: desktopbridge.ErrorUnavailable}
			if err := writeResponse(response); err != nil {
				shutdown()
				requests.Wait()
				return err
			}
			continue
		}
		requests.Add(1)
		go func(request desktopbridge.Request) {
			defer requests.Done()
			defer func() { <-requestSlots }()
			if err := writeResponse(processor.Handle(processContext, request)); err != nil {
				select {
				case outputErrors <- err:
				default:
				}
			}
		}(request)
	}
}
