// Package firebaseapp centralizes Firebase Admin SDK initialization:
// Firestore client + Firebase Auth (replacing internal/auth/jwt.go).
//
// On Cloud Run, Application Default Credentials are used automatically
// (no service account JSON needed) as long as the Cloud Run service's
// runtime service account has the "Firebase Admin" / "Cloud Datastore User"
// IAM roles.
package firebaseapp

import (
	"context"
	"fmt"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
	"cloud.google.com/go/firestore"
)

type App struct {
	Firestore *firestore.Client
	Auth      *auth.Client
}

func New(ctx context.Context, projectID string) (*App, error) {
	conf := &firebase.Config{ProjectID: projectID}
	fbApp, err := firebase.NewApp(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("firebase.NewApp: %w", err)
	}

	fs, err := fbApp.Firestore(ctx)
	if err != nil {
		return nil, fmt.Errorf("fbApp.Firestore: %w", err)
	}

	authClient, err := fbApp.Auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("fbApp.Auth: %w", err)
	}

	return &App{Firestore: fs, Auth: authClient}, nil
}

// Claims mirrors the custom claims set on each Firebase Auth user, replacing
// the payload that used to be embedded in the custom JWT
// (internal/auth/jwt.go). Set via Auth.SetCustomUserClaims whenever a user's
// tenant/role assignment changes.
type Claims struct {
	UserID       string            `json:"-"` // from token.UID
	TenantID     string            `json:"tenant_id"`
	Roles        map[string]string `json:"roles"` // module -> role key
	IsSuperAdmin bool              `json:"is_super_admin"`
}

// VerifyIDToken replaces jwt.ParseAccessToken. The frontend sends the
// Firebase ID token (from `getIdToken()`) as a Bearer token; Firebase Auth
// handles signing/rotation/expiry, so internal/auth/jwt.go and the
// refresh-token-rotation dance can eventually be retired in favor of the
// client SDK's automatic token refresh — though the current PoC keeps the
// refresh_tokens collection for parity during the transition period.
func (a *App) VerifyIDToken(ctx context.Context, idToken string) (Claims, error) {
	token, err := a.Auth.VerifyIDToken(ctx, idToken)
	if err != nil {
		return Claims{}, fmt.Errorf("VerifyIDToken: %w", err)
	}

	tenantID, _ := token.Claims["tenant_id"].(string)
	isSuperAdmin, _ := token.Claims["is_super_admin"].(bool)

	roles := map[string]string{}
	if raw, ok := token.Claims["roles"].(map[string]interface{}); ok {
		for module, roleKey := range raw {
			if s, ok := roleKey.(string); ok {
				roles[module] = s
			}
		}
	}

	return Claims{
		UserID:       token.UID,
		TenantID:     tenantID,
		Roles:        roles,
		IsSuperAdmin: isSuperAdmin,
	}, nil
}

// SetUserClaims pushes tenant/role assignment to Firebase Auth so every
// subsequent ID token the client requests already carries them — this is
// what the Firestore Security Rules (see rules/firestore.rules) read via
// request.auth.token.*, and what the Go middleware reads without an extra
// Firestore round-trip per request.
func (a *App) SetUserClaims(ctx context.Context, uid string, claims Claims) error {
	return a.Auth.SetCustomUserClaims(ctx, uid, map[string]interface{}{
		"tenant_id":      claims.TenantID,
		"roles":          claims.Roles,
		"is_super_admin": claims.IsSuperAdmin,
	})
}
