package auth

import (
	"context"
	"net/http"
	"strings"
)

type ctxKey struct{}

// Middleware rejects requests without a valid "Authorization: Bearer" token and
// stores the token's claims in the request context.
func Middleware(v *Verifier, unauthorized http.Handler) func(http.Handler) http.Handler {
	return MiddlewareFunc(v.Verify, unauthorized)
}

// MiddlewareFunc is Middleware with a chosen token check.
func MiddlewareFunc(verify func(string) (Claims, error), unauthorized http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok {
				unauthorized.ServeHTTP(w, r)
				return
			}
			claims, err := verify(strings.TrimSpace(raw))
			if err != nil {
				unauthorized.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, claims)))
		})
	}
}

// FromContext returns the authenticated caller. ok is false outside Middleware.
func FromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(Claims)
	return c, ok
}
