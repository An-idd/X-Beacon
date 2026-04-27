package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/An-idd/x-beacon/internal/storage"
)

// secretPrefix mirrors the convention used by OpenAI / Anthropic so
// existing clients tooling-side ("redact tokens that start with sk-")
// works unchanged.
const secretPrefix = "sk-"

// secretBytes is the entropy length. 32 bytes → 43 chars after
// base64url-no-pad → 46 chars including the prefix. Comparable to OpenAI.
const secretBytes = 32

func runKeygen(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(stdout)
	in := registerDSNFlags(fs)
	name := fs.String("name", "", "human-readable label for the key (required)")
	idFlag := fs.String("id", "", "explicit key ID; default is a fresh UUIDv4")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *name == "" {
		return errors.New("--name is required (e.g. -name \"prod CI\")")
	}

	dsn, err := resolveDSN(in)
	if err != nil {
		return err
	}

	id := *idFlag
	if id == "" {
		id = uuid.NewString()
	}

	secret, err := generateSecret()
	if err != nil {
		return fmt.Errorf("generate secret: %w", err)
	}
	hash := sha256.Sum256([]byte(secret))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := storage.NewPool(ctx, storage.Config{DSN: dsn, MaxConns: 2})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id, key_hash, name) VALUES ($1, $2, $3)`,
		id, hash[:], *name,
	); err != nil {
		return fmt.Errorf("insert api_key: %w", err)
	}

	// Print the secret ONCE — it is never recoverable from the DB. The
	// banner is loud on purpose; ops users tend to copy lines, not paragraphs.
	fmt.Fprintf(stdout, "Created API key.\n")
	fmt.Fprintf(stdout, "  id:     %s\n", id)
	fmt.Fprintf(stdout, "  name:   %s\n", *name)
	fmt.Fprintf(stdout, "  secret: %s\n", secret)
	fmt.Fprintf(stdout, "\nThis secret is shown ONLY ONCE. Store it now; the gateway never displays it again.\n")
	return nil
}

// generateSecret emits a base64url-no-padding-encoded 32 random bytes
// prefixed with "sk-". 43 + 3 = 46 characters total.
func generateSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return secretPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}
