package credentialstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
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
	installationIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// Installation is the API-issued machine credential and gateway pairing.
// Its string forms deliberately omit the credential.
type Installation struct {
	EnvironmentOrigin   string `json:"environment_origin"`
	InstallationID      string `json:"installation_id"`
	MachineCredential   string `json:"machine_credential"`
	GatewayWebsocketURL string `json:"gateway_websocket_url"`
}

func (i Installation) String() string {
	return fmt.Sprintf("Installation{environment_origin:%q installation_id:%q machine_credential:<redacted> gateway_websocket_url:%q}", i.EnvironmentOrigin, i.InstallationID, i.GatewayWebsocketURL)
}

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
	if !validOrigin(installation.EnvironmentOrigin) || !installationIDPattern.MatchString(installation.InstallationID) || !validCredential(installation.MachineCredential) || !validGateway(installation.GatewayWebsocketURL, installation.EnvironmentOrigin) {
		return ErrInvalidInstallation
	}
	return nil
}

func validOrigin(origin string) bool {
	return origin == "https://my.personastack.ai" || origin == "https://personastack.ericgreer.info"
}

func validCredential(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(value, "="))
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(value)
	}
	return err == nil && len(decoded) == 32
}

func validGateway(raw, origin string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Path != "/v1/desktop-control/ws" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return false
	}
	if origin == "https://my.personastack.ai" {
		return parsed.Scheme == "wss" && strings.EqualFold(parsed.Hostname(), "cluster-agent.personastack.ai") && (parsed.Port() == "" || parsed.Port() == "443")
	}
	return origin == "https://personastack.ericgreer.info" && parsed.Scheme == "ws" && strings.EqualFold(parsed.Hostname(), "cluster-agent.personastack.lan") && (parsed.Port() == "" || parsed.Port() == "80")
}
