# KANTOR → Firebase Migration Plan

## 1. Architecture Decision

**Keep**: Go backend (chi router, handlers, services, middleware, RBAC engine, JWT logic if desired) deployed on **Cloud Run**.
**Replace**: Repository layer (raw SQL / pgx) → **Firestore** via `firebase.google.com/go/v4` Admin SDK.
**Replace**: Postgres RLS multi-tenancy → **Firestore Security Rules** + tenant-scoped collection paths.
**Replace**: Custom JWT (optional) → **Firebase Authentication** (custom claims for `tenant_id`, `role`, `permissions`).
**Replace**: SSE notifications → **Firestore real-time listeners** (`onSnapshot`) consumed directly by frontend, removing the need for a custom SSE endpoint.
**Replace**: DB-backed rate limiting → **Firestore counters** or Cloud Run + Firebase App Check / Cloud Armor.

Why Cloud Run instead of rewriting in Node/Python Cloud Functions: the existing 48k LOC of Go business logic (RBAC engine, validators, export/PDF/Excel generation, encryption, WhatsApp integration) stays untouched. Only the 39 repository files (~data access layer) need a full rewrite. This cuts total rewrite surface by roughly 70%.

## 2. Multi-tenancy model in Firestore

Postgres uses RLS with `current_setting('app.current_tenant')`. Firestore has no row-level policy engine, so tenant isolation is enforced by **path structure**, not by a `tenant_id` column filter:

```
/tenants/{tenantId}                          <- tenant document (was `tenants` table)
/tenants/{tenantId}/users/{userId}
/tenants/{tenantId}/roles/{roleId}
/tenants/{tenantId}/permissions/{permId}
/tenants/{tenantId}/user_roles/{userId}_{roleId}
/tenants/{tenantId}/audit_logs/{logId}
/tenants/{tenantId}/projects/{projectId}
/tenants/{tenantId}/projects/{projectId}/members/{userId}
/tenants/{tenantId}/kanban_columns/{colId}
/tenants/{tenantId}/kanban_tasks/{taskId}
/tenants/{tenantId}/assignment_rules/{ruleId}
/tenants/{tenantId}/departments/{deptId}
/tenants/{tenantId}/employees/{employeeId}
/tenants/{tenantId}/salaries/{salaryId}
/tenants/{tenantId}/bonuses/{bonusId}
/tenants/{tenantId}/compensation_policies/{policyId}
/tenants/{tenantId}/subscriptions/{subId}
/tenants/{tenantId}/subscription_alerts/{alertId}
/tenants/{tenantId}/finance_categories/{catId}
/tenants/{tenantId}/finance_records/{recId}
/tenants/{tenantId}/reimbursements/{reimbId}
/tenants/{tenantId}/notifications/{notifId}
/tenants/{tenantId}/campaigns/{campaignId}
/tenants/{tenantId}/campaign_columns/{colId}
/tenants/{tenantId}/campaign_attachments/{attId}
/tenants/{tenantId}/ads_metrics/{metricId}
/tenants/{tenantId}/leads/{leadId}
/tenants/{tenantId}/leads/{leadId}/activities/{activityId}
/tenants/{tenantId}/vps_servers/{serverId}
/tenants/{tenantId}/vps_apps/{appId}
/tenants/{tenantId}/vps_health_events/{eventId}
/tenants/{tenantId}/domains/{domainId}
/tenants/{tenantId}/domain_health_events/{eventId}
/tenants/{tenantId}/activity_sessions/{sessionId}
/tenants/{tenantId}/activity_entries/{entryId}
/tenants/{tenantId}/activity_consents/{userId}
/tenants/{tenantId}/tracker_reminder_configs/{cfgId}
/tenants/{tenantId}/wa_message_templates/{tplId}
/tenants/{tenantId}/wa_broadcast_schedules/{schedId}
/tenants/{tenantId}/wa_broadcast_logs/{logId}
/tenants/{tenantId}/tenant_wa_configs/{configId}   (singleton doc)
/tenants/{tenantId}/oauth_clients/{clientId}
/tenants/{tenantId}/personal_access_tokens/{tokenId}
/tenants/{tenantId}/system_settings/{key}

-- Global (cross-tenant) collections --
/global_modules/{moduleId}                    (was `modules` — static catalog)
/global_permissions_v2/{permId}                (permission catalog, module:resource:action)
/global_refresh_tokens/{tokenId}               (or keep per-tenant if refresh tokens are tenant-scoped)
/global_password_reset_tokens/{tokenId}
/global_tenant_domains/{domain}                (maps custom domain -> tenantId, for tenant resolution on login)
```

Every Firestore Security Rule at the tenant subcollection level checks:
```
request.auth.token.tenant_id == tenantId
```
This is the Firestore equivalent of the Postgres RLS predicate.

## 3. RBAC mapping

Postgres `rbac_v2_revamp` migration introduced `roles_v2`, `permissions_v2`, `role_permissions_v2`, `user_module_roles` (module-scoped roles). Firestore equivalent:

- `permissions_v2` catalog → static, loaded once into `/global_permissions_v2`, cached in-memory by Go backend (same as today's `rbac/cache.go`).
- `user_module_roles` → stored as a **map field directly on the user's Firebase custom claims** for fast reads without extra Firestore lookups on every request:
  ```json
  { "tenant_id": "...", "roles": {"operational": "manager", "hris": "staff"}, "is_super_admin": false }
  ```
- Custom claims are refreshed (via Admin SDK `setCustomUserClaims`) whenever a role assignment changes — same pattern used today when refresh tokens are re-issued.

## 4. Encryption & audit logging

- AES-256-GCM at rest for `salaries`/`bonuses` (compensation) — **unchanged**, this logic lives in `internal/security` and is DB-agnostic; it just encrypts before writing the Firestore field and decrypts after reading it.
- `audit_logs` — written the same way, just via Firestore `Add()` instead of an `INSERT`. Composite queries (by user, module, date range) use Firestore indexes instead of Postgres B-tree indexes (`firestore.indexes.json`, generated separately per query pattern).

## 5. What does NOT map 1:1 (needs redesign, not just translation)

| Postgres feature | Problem in Firestore | Resolution |
|---|---|---|
| SQL JOINs (e.g. kanban_tasks + assignees + labels) | No joins | Denormalize: embed assignee summary + label array directly on the task document |
| Multi-table transactions (e.g. reimbursement approval touching `reimbursements` + `finance_records` + `audit_logs`) | Firestore transactions support up to 500 docs, no cross-collection joins mid-transaction, but batched writes work fine for this | Use Firestore `runTransaction` across the known doc paths |
| `ON CONFLICT ... DO UPDATE` (upsert) | No native upsert semantics beyond doc `set(merge:true)` | Use deterministic doc IDs (e.g. `{userId}_{roleId}`) so `set(merge:true)` behaves as upsert |
| SSE realtime notifications | No SSE needed | Replace entirely with Firestore `onSnapshot` listener in frontend — actually simpler |
| Full-text / complex filter queries (finance trend analysis, 12-month aggregation) | Firestore has weak aggregation (only count/sum/avg since 2023, no GROUP BY) | Pre-aggregate into a `monthly_summaries` subcollection updated via Cloud Function trigger on write, or run aggregation in Go backend after fetching raw docs |
| Row-Level Security | No RLS | Firestore Security Rules + tenant-path scoping (see §2) |
| Rate limiting table (per-tenant WA broadcast) | No row locking | Firestore transaction on a counter doc, or move to Cloud Run + Redis/Memorystore if throughput matters |

## 6. Phased delivery plan

| Phase | Scope | Status |
|---|---|---|
| 0 | Firebase project scaffold, Firestore rules skeleton, data model doc (this doc) | **Delivered below** |
| 1 | Core: users, roles/permissions (RBAC v2), tenants, audit_logs, refresh/PAT tokens | **PoC delivered below** |
| 2 | Operational: projects, kanban, assignment rules, tracker, VPS/domain monitoring | Next |
| 3 | HRIS: departments, employees, compensation (+encryption), subscriptions, finance, reimbursements | Next |
| 4 | Marketing: campaigns, ads metrics, leads | Next |
| 5 | Notifications (→ Firestore listeners), WhatsApp broadcast | Next |
| 6 | Frontend: swap `services/*.ts` from REST calls to Firebase SDK (or keep REST hitting Cloud Run, whichever the user prefers), Firebase Auth login flow | Next |
| 7 | Cutover: dual-write period, data backfill script (Postgres → Firestore), cleanup old Postgres code | Next |

Each phase after this one will be tackled in a follow-up turn — the full rewrite is too large for one response, and each module should be reviewed before moving to the next so nothing breaks silently.
