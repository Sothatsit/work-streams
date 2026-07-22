package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Sothatsit/work-stream/internal/config"
)

const (
	defaultPort       = "7139"
	dataEnvironment   = "WORK_STREAM_DATA"
	portEnvironment   = "WORK_STREAM_PORT"
	secretEnvironment = "WORK_STREAM_SECRET"
)

type serverConfig struct {
	port    string
	data    string
	timeout time.Duration
	secret  string
}

func parseConfig(
	args []string, getenv func(string) string,
) (serverConfig, error) {
	flags := flag.NewFlagSet("ws-server", flag.ContinueOnError)
	dataFlag := flags.String("data", "", "server data directory")
	portFlag := flags.String("port", "", "listen port")
	timeoutFlag := flags.String("timeout", "", "request timeout")
	if err := flags.Parse(args); err != nil {
		return serverConfig{}, err
	}
	if flags.NArg() != 0 {
		return serverConfig{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	data := *dataFlag
	if data == "" {
		data = getenv(dataEnvironment)
	}
	if data == "" {
		return serverConfig{}, fmt.Errorf(
			"data directory is required; use --data or %s",
			dataEnvironment,
		)
	}
	if !filepath.IsAbs(data) {
		return serverConfig{}, fmt.Errorf(
			"data directory must be an absolute path: %q", data,
		)
	}

	port := *portFlag
	if port == "" {
		port = getenv(portEnvironment)
	}
	if port == "" {
		port = defaultPort
	}

	timeout, err := config.ParseTimeout(
		*timeoutFlag, getenv(config.TimeoutEnvironment),
	)
	if err != nil {
		return serverConfig{}, err
	}
	secret := getenv(secretEnvironment)
	if err := validateSecret(secret); err != nil {
		return serverConfig{}, err
	}
	return serverConfig{
		port:    port,
		data:    filepath.Clean(data),
		timeout: timeout,
		secret:  secret,
	}, nil
}

func validateSecret(secret string) error {
	for i := 0; i < len(secret); i++ {
		if secret[i] <= ' ' || secret[i] == 0x7f {
			return fmt.Errorf(
				"%s contains whitespace or a control character",
				secretEnvironment,
			)
		}
	}
	return nil
}
