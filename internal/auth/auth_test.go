package auth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPrincipalFrom_NoValue_ReturnsNil(t *testing.T) {
	assert.Nil(t, PrincipalFrom(context.Background()))
}

func TestPrincipalFrom_RoundTrip(t *testing.T) {
	p := &Principal{ID: "k1", Name: "Test"}
	ctx := WithPrincipal(context.Background(), p)
	got := PrincipalFrom(ctx)
	assert.Same(t, p, got)
}

func TestPrincipalFrom_WrongType_ReturnsNil(t *testing.T) {
	// Defense: if some other package abused the same key with a different
	// value type, we should still return nil rather than panic.
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, "string-value")
	assert.Nil(t, PrincipalFrom(ctx))
}
