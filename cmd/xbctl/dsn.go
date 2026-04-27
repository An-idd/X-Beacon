package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/An-idd/x-beacon/internal/config"
)

// dsnFlags adds the standard --config / --dsn pair to fs and returns
// pointers the caller can pass to resolveDSN after fs.Parse.
//
// The pair is mutually exclusive in spirit but tolerated together: --dsn
// wins when both are set, so ops scripts can override a baked-in config
// path without editing the file.
type dsnInputs struct {
	configPath *string
	dsn        *string
}

func registerDSNFlags(fs *flag.FlagSet) dsnInputs {
	return dsnInputs{
		configPath: fs.String("config", "configs/config.yaml", "path to gateway config.yaml (DSN is read from database.dsn)"),
		dsn:        fs.String("dsn", "", "Postgres DSN; overrides -config when set"),
	}
}

// resolveDSN returns a usable DSN, preferring an explicit --dsn flag,
// then falling back to config.Database.DSN. Returns a friendly error
// when both are empty so the user gets a clear hint.
func resolveDSN(in dsnInputs) (string, error) {
	if d := *in.dsn; d != "" {
		return d, nil
	}
	cfg, err := config.Load(*in.configPath)
	if err != nil {
		return "", fmt.Errorf("load config %q: %w", *in.configPath, err)
	}
	if cfg.Database.DSN == "" {
		return "", errors.New("database.dsn is empty in config; pass -dsn or set it in config.yaml")
	}
	return cfg.Database.DSN, nil
}
