package testfixture

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDesktopParityFixtureMatchesMacResourceWhenBothReposAreCheckedOut(t *testing.T) {
	t.Parallel()
	macFixturePath := filepath.Clean("../../../../macos-desktop/Tests/PersonaStackTests/Fixtures/desktop-parity.json")
	macFixture, err := os.ReadFile(macFixturePath)
	if os.IsNotExist(err) {
		t.Skip("macos-desktop checkout is not adjacent to omarchy-desktop")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(macFixture, desktopParity) {
		t.Fatal("Mac and Linux desktop parity fixtures differ")
	}
	if _, err := LoadDesktopParity(); err != nil {
		t.Fatalf("decode shared desktop parity fixture: %v", err)
	}
}
