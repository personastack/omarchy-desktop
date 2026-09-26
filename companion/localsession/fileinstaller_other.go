//go:build !linux

package localsession

import (
	"context"

	"github.com/personastack/personastack-api/pkg/client/apicontract"
)

type unsupportedFileInstaller struct{}

func DefaultFileInstaller() (Installer, error) {
	return unsupportedFileInstaller{}, nil
}

func (unsupportedFileInstaller) Preflight(context.Context, string, HarnessInstallation) error {
	return ErrUnavailable
}

func (unsupportedFileInstaller) Configure(context.Context, apicontract.LocalSessionResponse, string, string, HarnessInstallation) error {
	return ErrUnavailable
}
