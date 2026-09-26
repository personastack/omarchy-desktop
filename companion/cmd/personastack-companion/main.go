package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/personastack/omarchy-desktop/companion/internal/credentialstore"
	"github.com/personastack/omarchy-desktop/companion/internal/desktopbridge"
	"github.com/personastack/omarchy-desktop/companion/internal/installation"
	"github.com/personastack/personastack-api/pkg/client/desktopcontrol"
)

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
	processor, err := desktopbridge.New(service, os.Args[1])
	if err != nil {
		os.Exit(2)
	}
	if err := serve(os.Stdin, os.Stdout, processor); err != nil {
		os.Exit(1)
	}
}

func serve(input io.Reader, output io.Writer, processor *desktopbridge.Processor) error {
	if processor == nil {
		return errors.New("desktop bridge unavailable")
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, desktopbridge.MaxRequestBytes), desktopbridge.MaxRequestBytes)
	writer := bufio.NewWriter(output)
	for scanner.Scan() {
		request, err := desktopbridge.Parse(scanner.Bytes())
		if err != nil {
			return err
		}
		response := processor.Handle(context.Background(), request)
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	}
	return scanner.Err()
}
