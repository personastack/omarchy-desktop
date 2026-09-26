package desktopexecutor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
)

func TestExecutorRequiresExclusiveScopedLeaseForCuaCalls(t *testing.T) {
	t.Parallel()
	runner := &runnerStub{response: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`)}
	executor, err := New(runner)
	if err != nil {
		t.Fatal(err)
	}
	command := commandFrame(agentgatewayruntime.DesktopControlOperationObserve, `{"control_token":"missing","tool":"get_desktop_state","arguments":{}}`)
	denied := executor.Handle(context.Background(), command, nil)
	if denied.Type != agentgatewayruntime.DesktopControlFrameFailure || runner.calls != 0 {
		t.Fatalf("unleased command = %#v, calls=%d", denied, runner.calls)
	}
	token := acquire(t, executor, commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	command.Arguments = json.RawMessage(`{"control_token":"` + token + `","tool":"get_desktop_state","arguments":{"display_id":"primary"}}`)
	response := executor.Handle(context.Background(), command, nil)
	if response.Type != agentgatewayruntime.DesktopControlFrameResult || string(response.Result) != string(runner.response) || runner.calls != 1 {
		t.Fatalf("Cua response = %#v, calls=%d", response, runner.calls)
	}
	wrongTool := command
	wrongTool.Operation = agentgatewayruntime.DesktopControlOperationObserve
	wrongTool.Arguments = json.RawMessage(`{"control_token":"` + token + `","tool":"click","arguments":{"x":1,"y":2}}`)
	if got := executor.Handle(context.Background(), wrongTool, nil); got.ErrorCode != "invalid_arguments" || runner.calls != 1 {
		t.Fatalf("operation/tool mismatch = %#v, calls=%d", got, runner.calls)
	}
	otherRun := command
	otherRun.Target = cloneTarget(command.Target)
	otherRun.Target.RunID = "other-run"
	otherRun.Arguments = json.RawMessage(`{"control_token":"` + token + `","tool":"get_desktop_state","arguments":{}}`)
	if got := executor.Handle(context.Background(), otherRun, nil); got.ErrorCode != "desktop_control_required" || runner.calls != 1 {
		t.Fatalf("other run used lease = %#v, calls=%d", got, runner.calls)
	}
	release := commandFrame(agentgatewayruntime.DesktopControlOperationRelease, `{"control_token":"`+token+`"}`)
	if got := executor.Handle(context.Background(), release, nil); got.Type != agentgatewayruntime.DesktopControlFrameResult || string(got.Result) != `{"released":true}` {
		t.Fatalf("producer-shaped release = %#v", got)
	}
}

func TestLocalOperationsRequireLeaseAndStripControlToken(t *testing.T) {
	t.Parallel()
	local := &localOperationsStub{response: json.RawMessage(`{"kind":"file"}`), closeOK: true}
	executor, err := NewWithLocalOperations(&runnerStub{response: json.RawMessage(`{}`)}, local)
	if err != nil {
		t.Fatal(err)
	}
	command := commandFrame(agentgatewayruntime.DesktopControlOperationFile,
		`{"control_token":"missing","action":"stat","path":"/tmp/a"}`)
	if got := executor.Handle(context.Background(), command, nil); got.ErrorCode != "desktop_control_required" || local.callCount() != 0 {
		t.Fatalf("unleased file operation=%#v calls=%d", got, local.callCount())
	}
	token := acquire(t, executor, commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	command.Arguments = json.RawMessage(`{"control_token":"` + token + `","action":"stat","path":"/tmp/a"}`)
	if got := executor.Handle(context.Background(), command, nil); got.Type != agentgatewayruntime.DesktopControlFrameResult {
		t.Fatalf("leased file operation=%#v", got)
	}
	var forwarded map[string]json.RawMessage
	if err := json.Unmarshal(local.arguments(), &forwarded); err != nil || forwarded["control_token"] != nil || string(forwarded["action"]) != `"stat"` {
		t.Fatalf("local operation arguments=%s err=%v", local.arguments(), err)
	}
	otherRun := command
	otherRun.Target = cloneTarget(command.Target)
	otherRun.Target.RunID = "other-run"
	if got := executor.Handle(context.Background(), otherRun, nil); got.ErrorCode != "desktop_control_required" || local.callCount() != 1 {
		t.Fatalf("other run used local operation=%#v calls=%d", got, local.callCount())
	}
}

func TestLocalOperationRevocationCancelsCallAndClosesResources(t *testing.T) {
	t.Parallel()
	local := &localOperationsStub{response: json.RawMessage(`{}`), closeOK: true, entered: make(chan struct{}), waitForContext: true}
	executor, err := NewWithLocalOperations(&runnerStub{response: json.RawMessage(`{}`)}, local)
	if err != nil {
		t.Fatal(err)
	}
	token := acquire(t, executor, commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	command := commandFrame(agentgatewayruntime.DesktopControlOperationFile,
		`{"control_token":"`+token+`","action":"search","root":"/tmp","name_contains":"x"}`)
	finished := make(chan agentgatewayruntime.DesktopControlFrame, 1)
	go func() { finished <- executor.Handle(context.Background(), command, nil) }()
	<-local.entered
	revoke := commandFrame(agentgatewayruntime.DesktopControlOperationRevokeConfig, `{}`)
	revoke.Target = &agentgatewayruntime.DesktopControlTarget{InstallationID: "install", WorkspaceID: "workspace", ConfigID: "config", ConfigVersion: 2}
	if got := executor.Handle(context.Background(), revoke, nil); got.Type != agentgatewayruntime.DesktopControlFrameResult {
		t.Fatalf("config revoke=%#v", got)
	}
	if got := <-finished; got.Type != agentgatewayruntime.DesktopControlFrameFailure || got.ErrorCode != "desktop_control_required" {
		t.Fatalf("late local result=%#v", got)
	}
	if local.closeCount() != 1 {
		t.Fatalf("local resource cleanup calls=%d", local.closeCount())
	}
}

func TestUnconfirmedLocalCleanupLeavesExecutorUnavailable(t *testing.T) {
	t.Parallel()
	local := &localOperationsStub{response: json.RawMessage(`{}`), closeOK: false}
	executor, err := NewWithLocalOperations(&runnerStub{response: json.RawMessage(`{}`)}, local)
	if err != nil {
		t.Fatal(err)
	}
	_ = acquire(t, executor, commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	revoke := commandFrame(agentgatewayruntime.DesktopControlOperationRevokeConfig, `{}`)
	revoke.Target = &agentgatewayruntime.DesktopControlTarget{InstallationID: "install", WorkspaceID: "workspace", ConfigID: "config", ConfigVersion: 2}
	if got := executor.Handle(context.Background(), revoke, nil); got.ErrorCode != "desktop_control_revoke_incomplete" {
		t.Fatalf("unconfirmed cleanup revoke=%#v", got)
	}
	newLease := commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`)
	newLease.Target = cloneTarget(newLease.Target)
	newLease.Target.ConfigVersion = 3
	if got := executor.Handle(context.Background(), newLease, nil); got.ErrorCode != "desktop_executor_unavailable" {
		t.Fatalf("acquire after unconfirmed cleanup=%#v", got)
	}
}

func TestProcessChunksForwardOnlyWhileLeaseRemainsAuthorized(t *testing.T) {
	t.Parallel()
	local := &localOperationsStub{response: json.RawMessage(`{"execution_id":"run","chunks":[{"sequence":9,"stream":"stdout","data_base64":"aGVsbG8="}]}`), closeOK: true}
	executor, err := NewWithLocalOperations(&runnerStub{response: json.RawMessage(`{}`)}, local)
	if err != nil {
		t.Fatal(err)
	}
	token := acquire(t, executor, commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	command := commandFrame(agentgatewayruntime.DesktopControlOperationShellRead,
		`{"control_token":"`+token+`","execution_id":"run","cursor":0,"wait_ms":0}`)
	var chunks []agentgatewayruntime.DesktopControlFrame
	result := executor.Handle(context.Background(), command, func(frame agentgatewayruntime.DesktopControlFrame) error {
		chunks = append(chunks, frame)
		return nil
	})
	if result.Type != agentgatewayruntime.DesktopControlFrameResult || len(chunks) != 1 || chunks[0].Type != agentgatewayruntime.DesktopControlFrameResultChunk || string(chunks[0].StreamData) != "hello" {
		t.Fatalf("result=%#v chunks=%#v", result, chunks)
	}
}

func TestExecutorRevocationFencesInFlightResult(t *testing.T) {
	t.Parallel()
	runner := &runnerStub{response: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), entered: make(chan struct{}), release: make(chan struct{})}
	executor, err := New(runner)
	if err != nil {
		t.Fatal(err)
	}
	acquireFrame := commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`)
	token := acquire(t, executor, acquireFrame)
	if repeated := acquire(t, executor, acquireFrame); repeated != token {
		t.Fatalf("same-owner reacquire changed token")
	}
	command := commandFrame(agentgatewayruntime.DesktopControlOperationObserve, `{"control_token":"`+token+`","tool":"get_desktop_state","arguments":{}}`)
	finished := make(chan agentgatewayruntime.DesktopControlFrame, 1)
	go func() { finished <- executor.Handle(context.Background(), command, nil) }()
	<-runner.entered
	revoke := commandFrame(agentgatewayruntime.DesktopControlOperationRevokeConfig, `{}`)
	revoke.Target = &agentgatewayruntime.DesktopControlTarget{InstallationID: "install", WorkspaceID: "workspace", ConfigID: "config", ConfigVersion: 3}
	if got := executor.Handle(context.Background(), revoke, nil); got.Type != agentgatewayruntime.DesktopControlFrameResult {
		t.Fatalf("revoke = %#v", got)
	}
	close(runner.release)
	if got := <-finished; got.Type != agentgatewayruntime.DesktopControlFrameFailure || got.ErrorCode != "desktop_control_required" {
		t.Fatalf("revoked in-flight result = %#v", got)
	}
}

func TestExecutorLeaseExpiresAndStatusStaysUnreadyUntilNativePathsExist(t *testing.T) {
	t.Parallel()
	now := time.Now()
	runner := &runnerStub{response: json.RawMessage(`{}`)}
	executor, err := New(runner)
	if err != nil {
		t.Fatal(err)
	}
	executor.now = func() time.Time { return now }
	token := acquire(t, executor, commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	status := executor.Handle(context.Background(), commandFrame(agentgatewayruntime.DesktopControlOperationStatus, `{}`), nil)
	if string(status.Result) != `{"available":false,"busy":true,"native_executor_ready":false}` || executor.NativeReady() {
		t.Fatalf("status=%s ready=%v", status.Result, executor.NativeReady())
	}
	now = now.Add(leaseIdleDuration + time.Second)
	command := commandFrame(agentgatewayruntime.DesktopControlOperationObserve, `{"control_token":"`+token+`","tool":"get_desktop_state","arguments":{}}`)
	command.DeadlineAt = now.Add(30 * time.Second)
	if got := executor.Handle(context.Background(), command, nil); got.ErrorCode != "desktop_control_required" || runner.calls != 0 {
		t.Fatalf("expired lease command = %#v, calls=%d", got, runner.calls)
	}
}

func TestExecutorLeaseHasHardMaximumLifetime(t *testing.T) {
	t.Parallel()
	now := time.Now()
	executor, err := New(&runnerStub{response: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	executor.now = func() time.Time { return now }
	token := acquire(t, executor, commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	executor.mu.Lock()
	executor.current.started = now.Add(-leaseMaxDuration - time.Second)
	executor.current.lastActivity = now
	executor.mu.Unlock()
	command := commandFrame(agentgatewayruntime.DesktopControlOperationObserve, `{"control_token":"`+token+`","tool":"get_desktop_state","arguments":{}}`)
	command.DeadlineAt = now.Add(30 * time.Second)
	if got := executor.Handle(context.Background(), command, nil); got.ErrorCode != "desktop_control_required" {
		t.Fatalf("expired maximum-lifetime lease = %#v", got)
	}
}

func TestLocalLeaseExpiryRenewsForActiveProcessThenCleansAtHardLimit(t *testing.T) {
	t.Parallel()
	now := time.Now()
	local := &localOperationsStub{response: json.RawMessage(`{}`), closeOK: true, activeProcesses: 1}
	executor, err := NewWithLocalOperations(&runnerStub{response: json.RawMessage(`{}`)}, local)
	if err != nil {
		t.Fatal(err)
	}
	executor.now = func() time.Time { return now }
	_ = acquire(t, executor, commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	now = now.Add(leaseIdleDuration + time.Second)
	executor.expireLeaseIfNeeded()
	if executor.current == nil || !executor.leaseValid(executor.current) || local.closeCount() != 0 {
		t.Fatalf("active process did not renew idle lease: lease=%#v closes=%d", executor.current, local.closeCount())
	}
	now = executor.current.started.Add(leaseMaxDuration)
	executor.expireLeaseIfNeeded()
	if executor.current != nil || local.closeCount() != 1 || executor.revocations != 0 {
		t.Fatalf("hard expiry did not clean lease resources: lease=%#v closes=%d revocations=%d", executor.current, local.closeCount(), executor.revocations)
	}
}

func TestShellStartTimeoutIsCappedToLeaseRemainingLifetime(t *testing.T) {
	t.Parallel()
	now := time.Now()
	local := &localOperationsStub{response: json.RawMessage(`{"execution_id":"test"}`), closeOK: true}
	executor, err := NewWithLocalOperations(&runnerStub{response: json.RawMessage(`{}`)}, local)
	if err != nil {
		t.Fatal(err)
	}
	executor.now = func() time.Time { return now }
	token := acquire(t, executor, commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	executor.mu.Lock()
	executor.current.started = now.Add(-leaseMaxDuration + 20*time.Second)
	executor.mu.Unlock()
	command := commandFrame(agentgatewayruntime.DesktopControlOperationShellStart,
		`{"control_token":"`+token+`","command":"sleep 5","working_directory":"/tmp","timeout_seconds":300}`)
	if got := executor.Handle(context.Background(), command, nil); got.Type != agentgatewayruntime.DesktopControlFrameResult {
		t.Fatalf("shell start=%#v", got)
	}
	if local.processTimeout <= 0 || local.processTimeout > 20*time.Second {
		t.Fatalf("process timeout exceeds lease remainder: %s", local.processTimeout)
	}
}

func TestExecutorBoundsOversizedCuaImageBeforeGatewayFrameEncoding(t *testing.T) {
	t.Parallel()
	imageData := noisyPNG(t, 1600, 1600)
	encodedImage := base64.StdEncoding.EncodeToString(imageData)
	if len(encodedImage) <= maxCuaImageBytes {
		t.Fatalf("fixture size=%d, need image larger than %d", len(encodedImage), maxCuaImageBytes)
	}
	response, err := json.Marshal(map[string]any{
		"content":           []any{map[string]any{"type": "image", "data": encodedImage, "mimeType": "image/png"}},
		"structuredContent": map[string]any{"screenshot_width": 1600, "screenshot_height": 1600, "screenshot_mime_type": "image/png", "scale_factor": 1.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &runnerStub{response: response}
	executor, err := New(runner)
	if err != nil {
		t.Fatal(err)
	}
	token := acquire(t, executor, commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	command := commandFrame(agentgatewayruntime.DesktopControlOperationObserve, `{"control_token":"`+token+`","tool":"get_desktop_state","arguments":{}}`)
	got := executor.Handle(context.Background(), command, nil)
	if got.Type != agentgatewayruntime.DesktopControlFrameResult {
		t.Fatalf("bounded image response = %#v", got)
	}
	if _, err := agentgatewayruntime.MarshalDesktopControlFrame(got); err != nil {
		t.Fatalf("bounded result is not a valid Gateway frame: %v", err)
	}
	var bounded struct {
		Content []struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			MIMEType string `json:"mimeType"`
		} `json:"content"`
		Structured map[string]json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(got.Result, &bounded); err != nil || len(bounded.Content) != 1 {
		t.Fatalf("decode bounded result: %v %#v", err, bounded)
	}
	if bounded.Content[0].MIMEType != "image/jpeg" || len(bounded.Content[0].Data) > maxCuaImageBytes || bounded.Structured["screenshot_mime_type"] == nil {
		t.Fatalf("large image was not normalized: content=%#v structured=%#v", bounded.Content[0], bounded.Structured)
	}
}

func TestScaleImageDoesNotAllocateAtFullResolution(t *testing.T) {
	// AllocsPerRun changes process-wide GOMAXPROCS while measuring allocations.
	source := image.NewRGBA(image.Rect(0, 0, 1, 1))
	source.SetRGBA(0, 0, color.RGBA{R: 10, G: 20, B: 30, A: 128})
	var scaled image.Image
	allocations := testing.AllocsPerRun(100, func() { scaled = scaleImage(source, 1) })
	if allocations != 0 {
		t.Fatalf("full-resolution image wrapper allocations = %v, want 0", allocations)
	}
	if got := scaled.Bounds(); got != image.Rect(0, 0, 1, 1) {
		t.Fatalf("full-resolution bounds = %v", got)
	}
	if got := color.RGBAModel.Convert(scaled.At(0, 0)).(color.RGBA); got != (color.RGBA{R: 10, G: 20, B: 30, A: 128}) {
		t.Fatalf("full-resolution pixel = %#v", got)
	}
}

func TestScaleImageFiltersHighContrastEdgesWhenDownscaling(t *testing.T) {
	t.Parallel()
	source := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for y := 0; y < 2; y++ {
		source.SetRGBA(0, y, color.RGBA{A: 255})
		source.SetRGBA(1, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		source.SetRGBA(2, y, color.RGBA{A: 255})
		source.SetRGBA(3, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	}
	scaled := scaleImage(source, .5)
	for x := 0; x < scaled.Bounds().Dx(); x++ {
		got := color.RGBAModel.Convert(scaled.At(x, 0)).(color.RGBA)
		if got.R < 127 || got.R > 128 || got.G != got.R || got.B != got.R || got.A != 255 {
			t.Fatalf("downscaled edge at x=%d = %#v, want opaque gray", x, got)
		}
	}
	oddSource := image.NewRGBA(image.Rect(0, 0, 7, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 7; x++ {
			oddSource.SetRGBA(x, y, color.RGBA{A: 255})
		}
		oddSource.SetRGBA(6, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	}
	oddScaled := scaleImage(oddSource, .5)
	lastPixel := color.RGBAModel.Convert(oddScaled.At(oddScaled.Bounds().Dx()-1, 0)).(color.RGBA)
	if oddScaled.Bounds() != image.Rect(0, 0, 3, 1) || lastPixel.R < 108 || lastPixel.R > 110 {
		t.Fatalf("odd-sized image edge: bounds=%v last pixel=%#v", oddScaled.Bounds(), lastPixel)
	}
}

func TestImageEncodingStopsWhenCommandContextIsCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	source := &cancelOnReadImage{Image: image.NewRGBA(image.Rect(0, 0, 256, 256)), cancel: cancel, cancelAt: 1500}
	var output bytes.Buffer
	err := encodeJPEGContext(ctx, &output, source, 70)
	if !errors.Is(err, context.Canceled) || source.reads <= source.cancelAt {
		t.Fatalf("encode cancellation err=%v reads=%d", err, source.reads)
	}
}

type cancelOnReadImage struct {
	image.Image
	cancel   context.CancelFunc
	cancelAt int
	reads    int
}

func (i *cancelOnReadImage) At(x, y int) color.Color {
	i.reads++
	if i.reads == i.cancelAt {
		i.cancel()
	}
	return i.Image.At(x, y)
}

func TestBindingRevocationDoesNotFenceOtherPersonas(t *testing.T) {
	t.Parallel()
	executor, err := New(&runnerStub{response: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	first := commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`)
	token := acquire(t, executor, first)
	revoke := commandFrame(agentgatewayruntime.DesktopControlOperationRevokeBinding, `{}`)
	revoke.Target = &agentgatewayruntime.DesktopControlTarget{InstallationID: "install", WorkspaceID: "workspace", ConfigID: "config", PersonaID: "persona", Generation: 1, ConfigVersion: 2}
	if got := executor.Handle(context.Background(), revoke, nil); got.Type != agentgatewayruntime.DesktopControlFrameResult {
		t.Fatalf("binding revoke = %#v", got)
	}
	first.Operation = agentgatewayruntime.DesktopControlOperationObserve
	first.Arguments = json.RawMessage(`{"control_token":"` + token + `","tool":"get_desktop_state","arguments":{}}`)
	if got := executor.Handle(context.Background(), first, nil); got.ErrorCode != "desktop_control_required" {
		t.Fatalf("revoked binding retained lease: %#v", got)
	}
	second := commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`)
	second.Target = cloneTarget(second.Target)
	second.Target.PersonaID = "another-persona"
	second.Target.RunID = "another-run"
	if got := executor.Handle(context.Background(), second, nil); got.Type != agentgatewayruntime.DesktopControlFrameResult {
		t.Fatalf("unrelated persona was fenced: %#v", got)
	}
}

func TestRevocationWaitsForActiveCommandBeforeAllowingAnotherLease(t *testing.T) {
	t.Parallel()
	runner := &stubbornRunner{entered: make(chan struct{}), release: make(chan struct{}), response: json.RawMessage(`{"content":[{"type":"text","text":"done"}]}`)}
	executor, err := New(runner)
	if err != nil {
		t.Fatal(err)
	}
	token := acquire(t, executor, commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`))
	command := commandFrame(agentgatewayruntime.DesktopControlOperationObserve, `{"control_token":"`+token+`","tool":"get_desktop_state","arguments":{}}`)
	commandDone := make(chan agentgatewayruntime.DesktopControlFrame, 1)
	go func() { commandDone <- executor.Handle(context.Background(), command, nil) }()
	<-runner.entered
	revoke := commandFrame(agentgatewayruntime.DesktopControlOperationRevokeConfig, `{}`)
	revoke.Target = &agentgatewayruntime.DesktopControlTarget{InstallationID: "install", WorkspaceID: "workspace", ConfigID: "config", ConfigVersion: 2}
	revoke.DeadlineAt = time.Now().Add(40 * time.Millisecond)
	if got := executor.Handle(context.Background(), revoke, nil); got.ErrorCode != "desktop_control_revoke_incomplete" {
		t.Fatalf("bounded revoke = %#v", got)
	}
	newLease := commandFrame(agentgatewayruntime.DesktopControlOperationAcquire, `{}`)
	newLease.Target = cloneTarget(newLease.Target)
	newLease.Target.ConfigVersion = 3
	if got := executor.Handle(context.Background(), newLease, nil); got.ErrorCode != "desktop_control_busy" {
		t.Fatalf("lease granted before command drained = %#v", got)
	}
	close(runner.release)
	if got := <-commandDone; got.ErrorCode != "desktop_control_required" {
		t.Fatalf("revoked command result = %#v", got)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := executor.Handle(context.Background(), newLease, nil); got.Type == agentgatewayruntime.DesktopControlFrameResult {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("executor remained fenced after the revoked command drained")
}

func TestStatusEnforcesRevocationAndEmptyRequestShape(t *testing.T) {
	t.Parallel()
	executor, err := New(&runnerStub{})
	if err != nil {
		t.Fatal(err)
	}
	revoke := commandFrame(agentgatewayruntime.DesktopControlOperationRevokeConfig, `{}`)
	revoke.Target = &agentgatewayruntime.DesktopControlTarget{InstallationID: "install", WorkspaceID: "workspace", ConfigID: "config", ConfigVersion: 2}
	executor.Handle(context.Background(), revoke, nil)
	status := commandFrame(agentgatewayruntime.DesktopControlOperationStatus, `{}`)
	if got := executor.Handle(context.Background(), status, nil); got.ErrorCode != "desktop_control_config_revoked" {
		t.Fatalf("revoked status = %#v", got)
	}
	status.Target.ConfigVersion = 3
	status.Arguments = json.RawMessage(`{"extra":true}`)
	if got := executor.Handle(context.Background(), status, nil); got.ErrorCode != "invalid_arguments" {
		t.Fatalf("extra status field = %#v", got)
	}
}

type runnerStub struct {
	mu        sync.Mutex
	calls     int
	operation agentgatewayruntime.DesktopControlOperation
	name      string
	arguments json.RawMessage
	response  json.RawMessage
	entered   chan struct{}
	release   chan struct{}
}

type localOperationsStub struct {
	mu              sync.Mutex
	response        json.RawMessage
	callArgs        json.RawMessage
	calls           int
	closeCalls      int
	closeOK         bool
	activeProcesses int
	processTimeout  time.Duration
	entered         chan struct{}
	waitForContext  bool
}

func (local *localOperationsStub) Call(ctx context.Context, _ agentgatewayruntime.DesktopControlOperation, arguments json.RawMessage, processTimeout time.Duration) (json.RawMessage, error) {
	local.mu.Lock()
	local.calls++
	local.callArgs = append(json.RawMessage(nil), arguments...)
	local.processTimeout = processTimeout
	entered, waitForContext := local.entered, local.waitForContext
	local.mu.Unlock()
	if entered != nil {
		close(entered)
		if waitForContext {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	return append(json.RawMessage(nil), local.response...), nil
}

func (local *localOperationsStub) ActiveProcesses() int {
	local.mu.Lock()
	defer local.mu.Unlock()
	return local.activeProcesses
}

func (local *localOperationsStub) CloseAll(context.Context) bool {
	local.mu.Lock()
	defer local.mu.Unlock()
	local.closeCalls++
	return local.closeOK
}

func (local *localOperationsStub) callCount() int {
	local.mu.Lock()
	defer local.mu.Unlock()
	return local.calls
}

func (local *localOperationsStub) closeCount() int {
	local.mu.Lock()
	defer local.mu.Unlock()
	return local.closeCalls
}

func (local *localOperationsStub) arguments() json.RawMessage {
	local.mu.Lock()
	defer local.mu.Unlock()
	return append(json.RawMessage(nil), local.callArgs...)
}

type stubbornRunner struct {
	entered, release chan struct{}
	response         json.RawMessage
}

func (r *stubbornRunner) Call(context.Context, agentgatewayruntime.DesktopControlOperation, string, json.RawMessage) (json.RawMessage, error) {
	close(r.entered)
	<-r.release
	return append(json.RawMessage(nil), r.response...), nil
}

func (r *runnerStub) Call(ctx context.Context, operation agentgatewayruntime.DesktopControlOperation, name string, arguments json.RawMessage) (json.RawMessage, error) {
	r.mu.Lock()
	r.calls++
	r.operation = operation
	r.name = name
	r.arguments = append(json.RawMessage(nil), arguments...)
	r.mu.Unlock()
	if r.entered != nil {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return append(json.RawMessage(nil), r.response...), nil
}

func commandFrame(operation agentgatewayruntime.DesktopControlOperation, arguments string) agentgatewayruntime.DesktopControlFrame {
	return agentgatewayruntime.DesktopControlFrame{
		Version: agentgatewayruntime.DesktopControlProtocolVersion, Type: agentgatewayruntime.DesktopControlFrameCommand,
		RequestID: "request-1", Target: &agentgatewayruntime.DesktopControlTarget{InstallationID: "install", WorkspaceID: "workspace", ConfigID: "config", PersonaID: "persona", RunID: "run", Generation: 1, ConfigVersion: 2},
		Operation: operation, Arguments: json.RawMessage(arguments), DeadlineAt: time.Now().Add(30 * time.Second),
	}
}

func acquire(t *testing.T, executor *Executor, frame agentgatewayruntime.DesktopControlFrame) string {
	t.Helper()
	response := executor.Handle(context.Background(), frame, nil)
	if response.Type != agentgatewayruntime.DesktopControlFrameResult {
		t.Fatalf("acquire = %#v", response)
	}
	var payload struct {
		ControlToken string `json:"control_token"`
	}
	if json.Unmarshal(response.Result, &payload) != nil || payload.ControlToken == "" {
		t.Fatalf("invalid lease result: %s", response.Result)
	}
	return payload.ControlToken
}

func cloneTarget(target *agentgatewayruntime.DesktopControlTarget) *agentgatewayruntime.DesktopControlTarget {
	copy := *target
	return &copy
}

func noisyPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	imageData := image.NewRGBA(image.Rect(0, 0, width, height))
	state := uint32(1)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			state = state*1664525 + 1013904223
			imageData.SetRGBA(x, y, color.RGBA{R: uint8(state >> 24), G: uint8(state >> 16), B: uint8(state >> 8), A: 255})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, imageData); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
