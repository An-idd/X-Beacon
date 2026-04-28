package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
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

// scopeFlag accumulates -scope category:value occurrences. Multiple
// values per category collapse into one array.
type scopeFlag map[string][]string

func (s *scopeFlag) String() string { return fmt.Sprintf("%v", map[string][]string(*s)) }

func (s *scopeFlag) Set(raw string) error {
	idx := strings.IndexByte(raw, ':')
	if idx <= 0 || idx == len(raw)-1 {
		return fmt.Errorf("invalid scope %q (expected category:value)", raw)
	}
	if *s == nil {
		*s = scopeFlag{}
	}
	cat, val := raw[:idx], raw[idx+1:]
	(*s)[cat] = append((*s)[cat], val)
	return nil
}

func runKeygen(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(stdout)
	in := registerDSNFlags(fs)
	name := fs.String("name", "", "human-readable label for the key (required)")
	idFlag := fs.String("id", "", "explicit key ID; default is a fresh UUIDv4")
	scopes := scopeFlag{}
	fs.Var(&scopes, "scope", "grant scope category:value (repeatable; e.g. -scope admin:pricing)")
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

	scopesJSON := []byte(`{}`)
	if len(scopes) > 0 {
		scopesJSON, err = json.Marshal(scopes)
		if err != nil {
			return fmt.Errorf("encode scopes: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := storage.NewPool(ctx, storage.Config{DSN: dsn, MaxConns: 2})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id, key_hash, name, scopes) VALUES ($1, $2, $3, $4)`,
		id, hash[:], *name, scopesJSON,
	); err != nil {
		return fmt.Errorf("insert api_key: %w", err)
	}

	// Print the secret ONCE — it is never recoverable from the DB. The
	// banner is loud on purpose; ops users tend to copy lines, not paragraphs.
	fmt.Fprintf(stdout, "Created API key.\n")
	fmt.Fprintf(stdout, "  id:     %s\n", id)
	fmt.Fprintf(stdout, "  name:   %s\n", *name)
	if len(scopes) > 0 {
		fmt.Fprintf(stdout, "  scopes: %s\n", scopesJSON)
	}
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
