package credentialstore

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretServiceStoresAndReadsOriginScopedInstallation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	secretPath := filepath.Join(dir, "secret")
	tool := fakeSecretTool(t, argsPath, secretPath, `
case "$1" in
  store) cat > "$SECRET_PATH" ;;
  lookup) cat "$SECRET_PATH" ;;
  clear) exit 0 ;;
  *) exit 2 ;;
esac
`)
	store := SecretService{command: tool}
	installation := testInstallation()
	if err := store.Save(context.Background(), installation); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	stored, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("read stored secret: %v", err)
	}
	if !strings.Contains(string(stored), installation.MachineCredential) {
		t.Fatal("Secret Service stdin did not receive the API credential")
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read arguments: %v", err)
	}
	if strings.Contains(string(args), installation.MachineCredential) {
		t.Fatal("machine credential was exposed in secret-tool arguments")
	}
	for _, expected := range []string{"store", "application", serviceName, "environment_origin", installation.EnvironmentOrigin} {
		if !strings.Contains(string(args), expected) {
			t.Errorf("secret-tool arguments omit %q: %s", expected, args)
		}
	}
	if strings.Contains(string(args), installation.InstallationID) {
		t.Errorf("installation ID should stay inside the origin-scoped secret, not become a keyring selector: %s", args)
	}
	loaded, err := store.Load(context.Background(), installation.EnvironmentOrigin)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded == nil || *loaded != installation {
		t.Fatalf("Load() = %#v, want %#v", loaded, installation)
	}
	if strings.Contains(installation.String(), installation.MachineCredential) {
		t.Fatal("Installation.String() exposed the machine credential")
	}
}

func TestSecretServiceDeletesOnlyTheSelectedInstallation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	secretPath := filepath.Join(dir, "secret")
	store := SecretService{command: fakeSecretTool(t, argsPath, secretPath, `
case "$1" in
  clear) printf '%s\n' "$@" > "$ARGS_PATH" ;;
  *) exit 2 ;;
esac
`)}
	installation := testInstallation()
	if err := store.Delete(context.Background(), installation); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read arguments: %v", err)
	}
	for _, expected := range []string{"clear", "environment_origin", installation.EnvironmentOrigin} {
		if !strings.Contains(string(args), expected) {
			t.Errorf("clear arguments omit %q: %s", expected, args)
		}
	}
	if strings.Contains(string(args), installation.InstallationID) {
		t.Errorf("installation ID should not be passed to secret-tool: %s", args)
	}
}

func TestSecretServiceFailsClosedForMissingOrUnavailableKeyring(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "missing", body: `exit 1`, wantErr: ErrCredentialMissing},
		{name: "unavailable", body: `echo locked >&2; exit 1`, wantErr: ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := SecretService{command: fakeSecretTool(t, filepath.Join(t.TempDir(), "args"), filepath.Join(t.TempDir(), "secret"), tc.body)}
			_, err := store.Load(context.Background(), "https://my.personastack.ai")
			if err != tc.wantErr {
				t.Fatalf("Load() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestSecretServiceRejectsInvalidOriginCredentialAndGatewayBeforeCommands(t *testing.T) {
	t.Parallel()
	commandPath := filepath.Join(t.TempDir(), "must-not-run")
	installation := testInstallation()
	for _, tc := range []struct {
		name         string
		installation Installation
	}{
		{name: "unapproved origin", installation: withOrigin(installation, "https://attacker.example", "wss://attacker.example/v1/desktop-control/ws")},
		{name: "bad credential", installation: withCredential(installation, "secret")},
		{name: "cross-origin gateway", installation: withGateway(installation, "wss://attacker.example/v1/desktop-control/ws")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := SecretService{command: commandPath}
			if err := store.Save(context.Background(), tc.installation); err != ErrInvalidInstallation {
				t.Fatalf("Save() error = %v, want %v", err, ErrInvalidInstallation)
			}
		})
	}
}

func TestSecretServiceRejectsCancelledOperationWithoutLeakingCommandOutput(t *testing.T) {
	t.Parallel()
	tool := fakeSecretTool(t, filepath.Join(t.TempDir(), "args"), filepath.Join(t.TempDir(), "secret"), `sleep 2`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (SecretService{command: tool}).Save(ctx, testInstallation())
	if err != ErrUnavailable {
		t.Fatalf("Save() error = %v, want %v", err, ErrUnavailable)
	}
}

func fakeSecretTool(t *testing.T, argsPath, secretPath, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret-tool")
	argsVar := shellQuote(argsPath)
	secretVar := shellQuote(secretPath)
	script := "#!/bin/sh\nARGS_PATH=" + argsVar + "\nSECRET_PATH=" + secretVar + "\nprintf '%s\\n' \"$@\" > \"$ARGS_PATH\"\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake secret-tool: %v", err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func testInstallation() Installation {
	return Installation{
		EnvironmentOrigin:   "https://my.personastack.ai",
		InstallationID:      "install_01",
		MachineCredential:   base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		GatewayWebsocketURL: "wss://cluster-agent.personastack.ai/v1/desktop-control/ws",
	}
}

func withOrigin(i Installation, origin, gateway string) Installation {
	i.EnvironmentOrigin = origin
	i.GatewayWebsocketURL = gateway
	return i
}

func withCredential(i Installation, credential string) Installation {
	i.MachineCredential = credential
	return i
}

func withGateway(i Installation, gateway string) Installation {
	i.GatewayWebsocketURL = gateway
	return i
}
