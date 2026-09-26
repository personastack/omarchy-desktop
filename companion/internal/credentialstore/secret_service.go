package credentialstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"time"

	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

const (
	secretToolPath = "/usr/bin/secret-tool"
	commandTimeout = 30 * time.Second
	maxSecretBytes = 8 * 1024
	serviceName    = "personastack-desktop-control"
)

var (
	ErrInvalidInstallation = errors.New("invalid desktop control installation")
	ErrCredentialMissing   = errors.New("desktop control installation is not enrolled")
	ErrUnavailable         = errors.New("Linux Secret Service is unavailable")
)

// Installation is the producer-owned API contract for an enrolled desktop.
type Installation = desktopcontrol.Installation

// SecretService stores one API-owned installation per app origin in the
// user's Linux Secret Service collection. It has no plaintext fallback.
type SecretService struct {
	command string
}

func NewSecretService() SecretService {
	return SecretService{command: secretToolPath}
}

func (s SecretService) Save(ctx context.Context, installation Installation) error {
	if err := validateInstallation(installation); err != nil {
		return err
	}
	secret, err := json.Marshal(installation)
	if err != nil || len(secret) > maxSecretBytes {
		return ErrInvalidInstallation
	}
	args := append([]string{"store", "--label=PersonaStack Desktop Control"}, attributes(installation.EnvironmentOrigin)...)
	if _, err := s.run(ctx, args, secret, false); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (s SecretService) Load(ctx context.Context, origin string) (*Installation, error) {
	if !validOrigin(origin) {
		return nil, ErrInvalidInstallation
	}
	args := append([]string{"lookup"}, attributes(origin)...)
	result, err := s.run(ctx, args, nil, true)
	if errors.Is(err, errNotFound) {
		return nil, ErrCredentialMissing
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	var installation Installation
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSuffix(result, []byte("\n"))))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&installation); err != nil || decoder.Decode(new(any)) != io.EOF || validateInstallation(installation) != nil || installation.EnvironmentOrigin != origin {
		return nil, ErrUnavailable
	}
	return &installation, nil
}

func (s SecretService) Delete(ctx context.Context, installation Installation) error {
	if err := validateInstallation(installation); err != nil {
		return err
	}
	args := append([]string{"clear"}, attributes(installation.EnvironmentOrigin)...)
	if _, err := s.run(ctx, args, nil, false); err != nil {
		return ErrUnavailable
	}
	return nil
}

var errNotFound = errors.New("secret not found")

func (s SecretService) run(parent context.Context, args []string, stdin []byte, capture bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.command, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr boundedBuffer
	stdout.limit = maxSecretBytes + 1
	stderr.limit = 1024
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil || stdout.overflow || errors.Is(err, exec.ErrNotFound) {
			return nil, ErrUnavailable
		}
		var exitErr *exec.ExitError
		if capture && errors.As(err, &exitErr) && stdout.Len() == 0 && stderr.Len() == 0 {
			return nil, errNotFound
		}
		return nil, ErrUnavailable
	}
	if stdout.overflow {
		return nil, ErrUnavailable
	}
	if capture {
		return bytes.Clone(stdout.Bytes()), nil
	}
	return nil, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	originalLength := len(p)
	if b.Len()+len(p) > b.limit {
		b.overflow = true
		p = p[:max(0, b.limit-b.Len())]
	}
	_, _ = b.Buffer.Write(p)
	return originalLength, nil
}

func attributes(origin string) []string {
	return []string{"application", serviceName, "environment_origin", origin}
}

func validateInstallation(installation Installation) error {
	if installation.Validate(installation.EnvironmentOrigin) != nil {
		return ErrInvalidInstallation
	}
	return nil
}

func validOrigin(origin string) bool {
	_, err := desktopcontrol.New(origin)
	return err == nil
}
