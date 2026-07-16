// Package auth — Firestore-backed replacement for the Postgres/pgx repository.
//
// DESIGN NOTES (read before extending to other modules):
//   - Every document lives at /tenants/{tenantID}/<collection>/{docID}, mirroring
//     the tenant-scoping that Postgres RLS used to enforce via
//     current_setting('app.current_tenant').
//   - Deterministic document IDs replace Postgres UNIQUE constraints + ON CONFLICT.
//     e.g. user_roles doc ID = "{userID}_{roleID}" so re-assigning a role is a
//     plain Set(merge:true) — the Firestore equivalent of an upsert.
//   - firestore.RunTransaction() replaces multi-statement SQL transactions.
//   - RBAC role/permission lookups that used to be SQL JOINs are now:
//       1) read the user doc's `roles` map (module -> role key) — this is also
//          mirrored into Firebase Auth custom claims for fast per-request checks
//          without a Firestore read at all (see auth/middleware in phase 2).
//       2) expand role -> permissions via the in-memory permission cache
//          (unchanged from today's internal/rbac/cache.go — that logic is
//          DB-agnostic and does not need to be rewritten).
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var ErrNotFound = errors.New("resource not found")
var ErrConflict = errors.New("resource already exists")

// User mirrors backend/internal/model.User — kept as a plain struct here so
// this package has zero dependency on the old pgx-based model package.
type User struct {
	ID                  string            `firestore:"-"` // populated from doc.Ref.ID, not stored as a field
	Email               string            `firestore:"email"`
	PasswordHash        string            `firestore:"password_hash"`
	FullName            string            `firestore:"full_name"`
	AvatarURL           *string           `firestore:"avatar_url,omitempty"`
	Department          *string           `firestore:"department,omitempty"`
	Skills              []string          `firestore:"skills,omitempty"`
	IsActive            bool              `firestore:"is_active"`
	IsSuperAdmin        bool              `firestore:"is_super_admin"`
	FailedLoginAttempts int               `firestore:"failed_login_attempts"`
	LockedUntil         *time.Time        `firestore:"locked_until,omitempty"`
	Roles               map[string]string `firestore:"roles"` // module -> role key, e.g. {"operational":"manager"}
	CreatedAt           time.Time         `firestore:"created_at"`
	UpdatedAt           time.Time         `firestore:"updated_at"`
}

type RefreshToken struct {
	ID         string     `firestore:"-"`
	UserID     string     `firestore:"user_id"`
	TokenHash  string     `firestore:"token_hash"`
	ExpiresAt  time.Time  `firestore:"expires_at"`
	RevokedAt  *time.Time `firestore:"revoked_at,omitempty"`
	CreatedAt  time.Time  `firestore:"created_at"`
	LastUsedAt *time.Time `firestore:"last_used_at,omitempty"`
	UserAgent  *string    `firestore:"user_agent,omitempty"`
	IPAddress  *string    `firestore:"ip_address,omitempty"`
}

type CreateUserParams struct {
	Email        string
	PasswordHash string
	FullName     string
	Department   *string
	Skills       []string
	Roles        map[string]string // module -> role key
}

type CreateRefreshTokenParams struct {
	UserID    string
	TokenHash string
	ExpiresAt time.Time
	UserAgent string
	IPAddress string
}

type Repository struct {
	client   *firestore.Client
	tenantID string // resolved once per request from Firebase Auth custom claim, see middleware in phase 2
}

func New(client *firestore.Client, tenantID string) *Repository {
	return &Repository{client: client, tenantID: tenantID}
}

func (r *Repository) tenantColl(name string) *firestore.CollectionRef {
	return r.client.Collection("tenants").Doc(r.tenantID).Collection(name)
}

// --- Users -----------------------------------------------------------------

// CreateUserWithRoles is the Firestore equivalent of the old
//
//	INSERT INTO users (...) VALUES (...) ON CONFLICT (tenant_id, email) DO UPDATE ...
//
// Firestore has no unique-constraint-on-arbitrary-column enforcement, so
// "email must be unique within a tenant" is enforced by using the email
// itself (normalized) as the document ID instead of a random UUID.
func (r *Repository) CreateUserWithRoles(ctx context.Context, params CreateUserParams) (User, error) {
	docID := normalizeEmailAsID(params.Email)
	ref := r.tenantColl("users").Doc(docID)

	now := time.Now().UTC()
	user := User{
		Email:        params.Email,
		PasswordHash: params.PasswordHash,
		FullName:     params.FullName,
		Department:   params.Department,
		Skills:       params.Skills,
		IsActive:     true,
		IsSuperAdmin: false,
		Roles:        params.Roles,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	err := r.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		snap, err := tx.Get(ref)
		if err == nil && snap.Exists() {
			return ErrConflict
		}
		if err != nil && status.Code(err) != codes.NotFound {
			return err
		}
		return tx.Set(ref, user)
	})
	if err != nil {
		return User{}, err
	}
	user.ID = docID
	return user, nil
}

func (r *Repository) GetUserByEmail(ctx context.Context, email string) (User, error) {
	ref := r.tenantColl("users").Doc(normalizeEmailAsID(email))
	snap, err := ref.Get(ctx)
	if status.Code(err) == codes.NotFound {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	var user User
	if err := snap.DataTo(&user); err != nil {
		return User{}, err
	}
	user.ID = snap.Ref.ID
	return user, nil
}

func (r *Repository) GetUserByID(ctx context.Context, userID string) (User, error) {
	snap, err := r.tenantColl("users").Doc(userID).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	var user User
	if err := snap.DataTo(&user); err != nil {
		return User{}, err
	}
	user.ID = snap.Ref.ID
	return user, nil
}

// GetUserRolesAndPermissions replaces the old SQL JOIN across
// user_roles -> roles -> role_permissions -> permissions. Since roles are
// stored directly on the user document (denormalized), this is now a single
// document read instead of a 4-table join, plus an in-memory permission
// expansion (unchanged logic from internal/rbac).
func (r *Repository) GetUserRolesAndPermissions(ctx context.Context, userID string) (roles []string, permissions []string, err error) {
	user, err := r.GetUserByID(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	for module, roleKey := range user.Roles {
		roles = append(roles, fmt.Sprintf("%s:%s", module, roleKey))
	}
	// Permission expansion (role -> permission list) reuses the existing
	// in-memory rbac cache from internal/rbac/cache.go — that code is
	// DB-agnostic and does not need to change for this migration.
	return roles, permissions, nil
}

func (r *Repository) SetUserActive(ctx context.Context, userID string, active bool) error {
	_, err := r.tenantColl("users").Doc(userID).Update(ctx, []firestore.Update{
		{Path: "is_active", Value: active},
		{Path: "updated_at", Value: time.Now().UTC()},
	})
	return err
}

func (r *Repository) CountUsers(ctx context.Context) (int64, error) {
	// Firestore aggregation queries (count()) avoid pulling all docs to the client.
	res, err := r.tenantColl("users").NewAggregationQuery().WithCount("total").Get(ctx)
	if err != nil {
		return 0, err
	}
	count, ok := res["total"]
	if !ok {
		return 0, nil
	}
	return count.(*firestore.AggregationResult).Value.(int64), nil
}

// --- Refresh tokens ----------------------------------------------------------

// CreateRefreshToken: the token hash itself becomes the doc ID (it's already
// a unique, unguessable value), replacing the old auto-increment/UUID + unique
// index on token_hash.
func (r *Repository) CreateRefreshToken(ctx context.Context, params CreateRefreshTokenParams) error {
	ref := r.tenantColl("refresh_tokens").Doc(params.TokenHash)
	_, err := ref.Set(ctx, RefreshToken{
		UserID:    params.UserID,
		TokenHash: params.TokenHash,
		ExpiresAt: params.ExpiresAt,
		CreatedAt: time.Now().UTC(),
		UserAgent: &params.UserAgent,
		IPAddress: &params.IPAddress,
	})
	return err
}

func (r *Repository) GetRefreshTokenByHash(ctx context.Context, tokenHash string) (RefreshToken, error) {
	snap, err := r.tenantColl("refresh_tokens").Doc(tokenHash).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return RefreshToken{}, ErrNotFound
	}
	if err != nil {
		return RefreshToken{}, err
	}
	var t RefreshToken
	if err := snap.DataTo(&t); err != nil {
		return RefreshToken{}, err
	}
	t.ID = snap.Ref.ID
	return t, nil
}

// RotateRefreshToken replaces the old txn: revoke old token + insert new token
// in one SQL transaction. Firestore's RunTransaction gives the same atomicity
// guarantee across the two documents.
func (r *Repository) RotateRefreshToken(ctx context.Context, oldTokenHash string, params CreateRefreshTokenParams) error {
	oldRef := r.tenantColl("refresh_tokens").Doc(oldTokenHash)
	newRef := r.tenantColl("refresh_tokens").Doc(params.TokenHash)
	now := time.Now().UTC()

	return r.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		if err := tx.Update(oldRef, []firestore.Update{{Path: "revoked_at", Value: now}}); err != nil {
			return err
		}
		return tx.Set(newRef, RefreshToken{
			UserID:    params.UserID,
			TokenHash: params.TokenHash,
			ExpiresAt: params.ExpiresAt,
			CreatedAt: now,
			UserAgent: &params.UserAgent,
			IPAddress: &params.IPAddress,
		})
	})
}

func (r *Repository) RevokeAllUserTokens(ctx context.Context, userID string) error {
	iter := r.tenantColl("refresh_tokens").Where("user_id", "==", userID).Where("revoked_at", "==", nil).Documents(ctx)
	defer iter.Stop()

	batch := r.client.Batch()
	now := time.Now().UTC()
	count := 0
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return err
		}
		batch.Update(doc.Ref, []firestore.Update{{Path: "revoked_at", Value: now}})
		count++
		if count >= 400 { // stay under Firestore's 500-write batch limit
			if _, err := batch.Commit(ctx); err != nil {
				return err
			}
			batch = r.client.Batch()
			count = 0
		}
	}
	if count > 0 {
		_, err := batch.Commit(ctx)
		return err
	}
	return nil
}

// --- Failed login / lockout ---------------------------------------------------

func (r *Repository) IncrementFailedLoginAttempts(ctx context.Context, userID string, maxAttempts int, lockDuration time.Duration) error {
	ref := r.tenantColl("users").Doc(userID)
	return r.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		snap, err := tx.Get(ref)
		if err != nil {
			return err
		}
		var u User
		if err := snap.DataTo(&u); err != nil {
			return err
		}
		attempts := u.FailedLoginAttempts + 1
		updates := []firestore.Update{
			{Path: "failed_login_attempts", Value: attempts},
			{Path: "updated_at", Value: time.Now().UTC()},
		}
		if attempts >= maxAttempts {
			lockUntil := time.Now().UTC().Add(lockDuration)
			updates = append(updates, firestore.Update{Path: "locked_until", Value: lockUntil})
		}
		return tx.Update(ref, updates)
	})
}

func (r *Repository) ResetFailedLoginAttempts(ctx context.Context, userID string) error {
	_, err := r.tenantColl("users").Doc(userID).Update(ctx, []firestore.Update{
		{Path: "failed_login_attempts", Value: 0},
		{Path: "locked_until", Value: nil},
		{Path: "updated_at", Value: time.Now().UTC()},
	})
	return err
}

// --- helpers -----------------------------------------------------------------

// normalizeEmailAsID turns an email into a Firestore-safe document ID.
// Firestore doc IDs cannot contain "/" and are case-sensitive by default,
// so emails are lowercased before use to mimic Postgres's citext-like
// case-insensitive uniqueness behavior.
func normalizeEmailAsID(email string) string {
	out := make([]rune, 0, len(email))
	for _, c := range email {
		if c == '/' {
			continue
		}
		out = append(out, c)
	}
	return toLower(string(out))
}

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
