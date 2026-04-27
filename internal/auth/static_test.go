package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStatic_ValidEntries(t *testing.T) {
	s, err := NewStatic([]StaticEntry{
		{ID: "k1", Name: "first", Secret: "secret-one"},
		{ID: "k2", Name: "second", Secret: "secret-two"},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, s.Size())
}

func TestNewStatic_EmptySlice_Errors(t *testing.T) {
	_, err := NewStatic(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one")
}

func TestNewStatic_DuplicateID_Errors(t *testing.T) {
	_, err := NewStatic([]StaticEntry{
		{ID: "k1", Secret: "a"},
		{ID: "k1", Secret: "b"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate id")
}

func TestNewStatic_DuplicateSecret_Errors(t *testing.T) {
	// Same secret on two principals would silently mask one — must fail.
	_, err := NewStatic([]StaticEntry{
		{ID: "k1", Secret: "shared"},
		{ID: "k2", Secret: "shared"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides")
}

func TestNewStatic_MissingID_Errors(t *testing.T) {
	_, err := NewStatic([]StaticEntry{{Secret: "x"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "id is required")
}

func TestNewStatic_MissingSecret_Errors(t *testing.T) {
	_, err := NewStatic([]StaticEntry{{ID: "k1"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret is required")
}

func TestNewStatic_AggregatesErrors(t *testing.T) {
	_, err := NewStatic([]StaticEntry{
		{ID: ""},
		{Secret: ""},
		{ID: "k1", Secret: "x"},
		{ID: "k1", Secret: "y"},
	})
	require.Error(t, err)
	// errors.Join produces a multi-line message; smoke-check it surfaces
	// more than one underlying issue.
	assert.Contains(t, err.Error(), "id is required")
	assert.Contains(t, err.Error(), "duplicate id")
}

func TestStaticAuthenticator_AuthenticateValid(t *testing.T) {
	s, err := NewStatic([]StaticEntry{
		{ID: "dev", Name: "Local", Secret: "sk-local"},
	})
	require.NoError(t, err)

	p, err := s.Authenticate(context.Background(), "sk-local")
	require.NoError(t, err)
	assert.Equal(t, "dev", p.ID)
	assert.Equal(t, "Local", p.Name)
}

func TestStaticAuthenticator_AuthenticateEmpty(t *testing.T) {
	s, err := NewStatic([]StaticEntry{{ID: "dev", Secret: "x"}})
	require.NoError(t, err)

	_, err = s.Authenticate(context.Background(), "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMissingCredentials))
}

func TestStaticAuthenticator_AuthenticateInvalid(t *testing.T) {
	s, err := NewStatic([]StaticEntry{{ID: "dev", Secret: "right"}})
	require.NoError(t, err)

	_, err = s.Authenticate(context.Background(), "wrong")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidCredentials))
}
