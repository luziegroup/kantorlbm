// Package middleware — Firebase Auth replacement for internal/middleware/auth.go
// and internal/middleware/tenant.go. The chi router chain, handler signatures,
// and everything downstream (RBAC middleware, handlers) stay the same shape;
// only how the identity/tenant is extracted changes.
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/kana-consultant/kantor/backend/internal/firebaseapp"
)

type ctxKey string

const claimsCtxKey ctxKey = "firebase_claims"

func RequireAuth(app *firebaseapp.App) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			header := req.Header.Get("Authorization")
			if !strings.HasPrefix(header, "Bearer ") {
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			idToken := strings.TrimPrefix(header, "Bearer ")

			claims, err := app.VerifyIDToken(req.Context(), idToken)
			if err != nil {
				http.Error(w, "invalid or expired token", http.StatusUnauthorized)
				return
			}
			if claims.TenantID == "" {
				http.Error(w, "user has no tenant assigned", http.StatusForbidden)
				return
			}

			ctx := context.WithValue(req.Context(), claimsCtxKey, claims)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	}
}

func ClaimsFromContext(ctx context.Context) (firebaseapp.Claims, bool) {
	c, ok := ctx.Value(claimsCtxKey).(firebaseapp.Claims)
	return c, ok
}

// RequireModuleRole replaces internal/middleware/rbac.go's permission check.
// Because roles are already denormalized onto the custom claims (see
// firebaseapp.Claims), this is an in-memory map lookup — no Firestore read
// needed per request, same performance characteristic as the old in-memory
// rbac cache.
func RequireModuleRole(module string, allowed ...string) func(http.Handler) http.Handler {
	allowedSet := make(map[string]bool, len(allowed))
	for _, r := range allowed {
		allowedSet[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			claims, ok := ClaimsFromContext(req.Context())
			if !ok {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			if claims.IsSuperAdmin {
				next.ServeHTTP(w, req)
				return
			}
			role := claims.Roles[module]
			if !allowedSet[role] {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, req)
		})
	}
}
