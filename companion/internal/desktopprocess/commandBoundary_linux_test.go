//go:build linux

package desktopprocess

import (
	"context"
	"testing"
	"time"
)

func TestCommandTerminatorsPreserveExecution(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"printf marker;", "printf marker &", "printf marker # trailing comment", "printf marker\n"} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			manager := New("/bin/bash")
			t.Cleanup(func() {
				if !manager.CloseAll(context.Background()) {
					t.Error("process cleanup failed")
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := manager.Start(ctx, command, t.TempDir(), 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			chunks := append([]Chunk(nil), result.Chunks...)
			for result.State == StateRunning {
				result, err = manager.Read(ctx, result.ExecutionID, result.NextCursor, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				chunks = append(chunks, result.Chunks...)
			}
			if result.ExitCode == nil || *result.ExitCode != 0 || joinStream(chunks, "stdout") != "marker" {
				t.Fatalf("command failed: exit=%v stdout=%q stderr=%q", result.ExitCode, joinStream(chunks, "stdout"), joinStream(chunks, "stderr"))
			}
		})
	}
}
