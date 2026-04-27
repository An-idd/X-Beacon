package main

import (
	"errors"
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/provider/registry"
)

// loadAuth reads auth.yaml and constructs a static Authenticator,
// tolerating the file's absence at startup with a warning (so `make dev`
// works without writing an auth.yaml). Any other error — permissions,
// malformed YAML, validation — is fatal.
//
// Returns (nil, nil) when the file is absent. The server treats that as
// "auth disabled" and warns again at route mount time.
func loadAuth(path string, logger *zap.Logger) (auth.Authenticator, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Warn("auth file not found; /v1/* will be unauthenticated",
				zap.String("path", path),
				zap.String("hint", "copy configs/auth.example.yaml to enable bearer-token auth"))
			return nil, nil
		}
		return nil, fmt.Errorf("stat %q: %w", path, err)
	}
	authn, err := auth.Load(path)
	if err != nil {
		return nil, err
	}
	logger.Info("auth keys loaded", zap.Int("keys", authn.Size()))
	return authn, nil
}

// loadRegistry reads providers.yaml and constructs a Registry, tolerating
// the file's absence at startup with a clear warning. Any other error
// (permissions, malformed YAML, validation) is fatal.
//
// This stays in cmd/gateway because the absent-file warning is a
// startup-policy decision (zero-config dev experience), not server logic.
func loadRegistry(path string, logger *zap.Logger) (*registry.Registry, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Warn("providers file not found; gateway starts with empty registry",
				zap.String("path", path),
				zap.String("hint", "copy configs/providers.example.yaml and set API keys"))
			return registry.NewEmpty(), nil
		}
		return nil, fmt.Errorf("stat %q: %w", path, err)
	}
	reg, err := registry.Load(path)
	if err != nil {
		return nil, err
	}
	logger.Info("providers loaded",
		zap.Strings("names", reg.Names()),
		zap.Int("models", len(reg.AllModels())))
	return reg, nil
}
