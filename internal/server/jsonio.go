package server

import (
	"encoding/json"
	"io"
	"strconv"
)

// jsonEncode is a thin wrapper that writes JSON without a trailing newline.
// Centralized so handler code reads consistently and so tests can swap the
// behavior if we ever want pretty-printing in dev builds.
func jsonEncode(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func itoa(n int) string { return strconv.Itoa(n) }
