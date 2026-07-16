// cmd/backfill reads existing rows from the current Postgres database and
// writes them into Firestore in the new tenant-scoped shape described in
// docs/00-MIGRATION-PLAN.md. Run this ONCE per table, in dependency order
// (tenants first, then users/roles, then everything else), during the
// Phase 7 cutover window.
//
// This file is a skeleton for the `users` table only — the same pattern
// (SELECT * FROM <table> -> batch Firestore writes under
// /tenants/{tenantID}/<collection>) should be repeated for every remaining
// table listed in docs/00-MIGRATION-PLAN.md §2 once each module's Firestore
// repository (phase 2-5) is ready.
package main

import (
	"context"
	"log"
	"os"

	"cloud.google.com/go/firestore"
	firebase "firebase.google.com/go/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx := context.Background()

	pgPool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer pgPool.Close()

	fbApp, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: os.Getenv("FIREBASE_PROJECT_ID")})
	if err != nil {
		log.Fatalf("firebase init: %v", err)
	}
	fs, err := fbApp.Firestore(ctx)
	if err != nil {
		log.Fatalf("firestore client: %v", err)
	}
	defer fs.Close()

	if err := backfillUsers(ctx, pgPool, fs); err != nil {
		log.Fatalf("backfill users: %v", err)
	}
	log.Println("backfill complete")
}

func backfillUsers(ctx context.Context, pg *pgxpool.Pool, fs *firestore.Client) error {
	rows, err := pg.Query(ctx, `
		SELECT tenant_id, id, email, password_hash, full_name, department,
		       skills, is_active, is_super_admin, created_at, updated_at
		FROM users
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	batch := fs.Batch()
	count := 0

	for rows.Next() {
		var (
			tenantID, id, email, passwordHash, fullName string
			department                                  *string
			skills                                      []string
			isActive, isSuperAdmin                      bool
			createdAt, updatedAt                        interface{}
		)
		if err := rows.Scan(&tenantID, &id, &email, &passwordHash, &fullName,
			&department, &skills, &isActive, &isSuperAdmin, &createdAt, &updatedAt); err != nil {
			return err
		}

		docID := email // see repository.normalizeEmailAsID for the real transform
		ref := fs.Collection("tenants").Doc(tenantID).Collection("users").Doc(docID)
		batch.Set(ref, map[string]interface{}{
			"email":          email,
			"password_hash":  passwordHash,
			"full_name":      fullName,
			"department":     department,
			"skills":         skills,
			"is_active":      isActive,
			"is_super_admin": isSuperAdmin,
			"created_at":     createdAt,
			"updated_at":     updatedAt,
		})

		count++
		if count >= 400 {
			if _, err := batch.Commit(ctx); err != nil {
				return err
			}
			batch = fs.Batch()
			count = 0
		}
	}

	if count > 0 {
		if _, err := batch.Commit(ctx); err != nil {
			return err
		}
	}
	return rows.Err()
}
