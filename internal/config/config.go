package config

import (
	"fmt"
	"os"
	"time"
)

const TimeoutEnvironment = "WORK_STREAM_TIMEOUT"

const DefaultTimeout = 5 * time.Second

func Timeout(flagValue string) (time.Duration, error) {
	return ParseTimeout(flagValue, os.Getenv(TimeoutEnvironment))
}

func ParseTimeout(flagValue, environmentValue string) (time.Duration, error) {
	value := flagValue
	if value == "" {
		value = environmentValue
	}
	if value == "" {
		return DefaultTimeout, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid timeout %q: %w", value, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("timeout must be greater than zero")
	}
	return timeout, nil
}
