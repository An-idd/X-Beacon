// Package middleware contains the HTTP middleware chain that wraps every
// gateway request. Each middleware is a stand-alone function with the
// standard signature func(next http.Handler) http.Handler so order can be
// changed in cmd assembly without touching the implementations.
//
// Mounting order (outer → inner) and rationale:
//
//	Recovery  → catches panics from anything below
//	RequestID → makes req_id available for logs/traces below
//	Tracing   → opens a span around the handler chain
//	Logging   → captures final status/latency, picks up req_id + trace_id
//	(Auth, RateLimit, ... — added in later steps)
//
// Logging is innermost of these four because it writes the access log
// after the handler returns, when status and bytes-written are final.
package middleware
