package auth

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/An-idd/x-beacon/internal/envexpand"
)

// authFile is the on-disk schema of configs/auth.yaml.
type authFile struct {
	Keys []keyEntry `yaml:"keys"`
}

type keyEntry struct {
	ID     string `yaml:"id"`
	Name   string `yaml:"name"`
	Secret string `yaml:"secret"`
}

// Load reads auth.yaml from path, expands ${VAR} placeholders, and returns
// a StaticAuthenticator. Mirrors the registry.Load shape so main can treat
// auth/registry symmetrically.
//
// Empty or missing-after-expansion secrets cause Load to fail — there is
// no "skip silently" tolerance, because a placeholder like
// `secret: ${MISSING_ENV}` expanding to "" would otherwise silently drop
// a principal.
func Load(path string) (*StaticAuthenticator, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: read %q: %w", path, err)
	}

	expanded := envexpand.Expand(string(raw))

	var af authFile
	if err := yaml.Unmarshal([]byte(expanded), &af); err != nil {
		return nil, fmt.Errorf("auth: parse yaml: %w", err)
	}

	if len(af.Keys) == 0 {
		return nil, errors.New("auth: no keys defined in auth.yaml")
	}

	entries := make([]StaticEntry, len(af.Keys))
	for i, k := range af.Keys {
		entries[i] = StaticEntry{ID: k.ID, Name: k.Name, Secret: k.Secret}
	}

	authn, err := NewStatic(entries)
	if err != nil {
		return nil, fmt.Errorf("auth: build authenticator: %w", err)
	}
	return authn, nil
}
