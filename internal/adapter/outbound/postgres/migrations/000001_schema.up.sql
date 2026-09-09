-- Delegation Service schema (LLD §7.1-§7.6). One consolidated migration:
-- extensions, enums, both tenant tables, processed_events, the three-function
-- fail-closed RLS design (mirroring iam-user-profile's
-- app_tenant_id()/rls_check_tenant()/log_rls_violation()/rls_violation_log
-- pattern, NOT iam-tender-acl's simpler plain-policy pattern), the touch_row()
-- trigger, the delegation_app / delegation_migrator roles, and delegation_app's
-- grant on platform-events' outbox_events table (created by outbox.ApplySchema,
-- which cmd/server and cmd/reconciler both run before this migration — see
-- postgres.Migrate).

-- ── Extensions ────────────────────────────────────────────────────────────
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

-- ── Enum types (LLD §7.1) ─────────────────────────────────────────────────
CREATE TYPE public.delegation_scope  AS ENUM ('all', 'department', 'tender');
-- scheduled: cross-service future-OOO bug fix (DLG-D25) — a delegation
-- created with a future starts_at sits here until the delegation-activation
-- reconciler job flips it to active at starts_at; see domain.DelegationStatus.
CREATE TYPE public.delegation_status AS ENUM ('scheduled', 'active', 'ended', 'cancelled');

-- ── app_tenant_id() ───────────────────────────────────────────────────────
-- Reads the tenant GUC set by the pgcommon GUC bridge. STABLE (evaluates
-- once per statement), SECURITY DEFINER (invoker cannot poison the search
-- path), fail-closed (returns NULL on any error so RLS blocks rather than
-- silently opens).
CREATE OR REPLACE FUNCTION public.app_tenant_id() RETURNS uuid
    LANGUAGE plpgsql STABLE SECURITY DEFINER AS $$
DECLARE v text;
BEGIN
    v := current_setting('app.tenant_id', true);
    IF v IS NULL OR v = '' THEN RETURN NULL; END IF;
    RETURN v::uuid;
EXCEPTION WHEN OTHERS THEN RETURN NULL;
END;
$$;

-- ── delegations (LLD §7.2.1) ──────────────────────────────────────────────
-- From O&M, with the three cross-database FKs removed and
-- review_notice_sent_at replaced by review_last_warned_bucket (DLG-Q6): an
-- int (3|2|1|NULL) rather than a timestamp — the last days_remaining value
-- notified in the 3-day daily cascade, reset to NULL on extend/reassign to
-- re-arm the cascade.
CREATE TABLE public.delegations (
    id                        uuid NOT NULL DEFAULT gen_random_uuid(),
    tenant_id                 uuid NOT NULL,
    delegator_id              uuid NOT NULL,
    delegate_id               uuid NOT NULL,
    delegator_membership_id   uuid NOT NULL, -- composite-FK anchor (delegator); FK dropped on split (LLD §7.6.1)
    delegate_membership_id    uuid NOT NULL, -- composite-FK anchor (delegate);  FK dropped on split (LLD §7.6.1)
    scope                     public.delegation_scope NOT NULL DEFAULT 'all',
    scope_id                  uuid,
    reason                    text,
    starts_at                 timestamp with time zone NOT NULL DEFAULT now(),
    ends_at                   timestamp with time zone,          -- NULL = open-ended (DEL-8)
    review_due_at             timestamp with time zone,          -- open-ended only (DEL-13)
    review_last_warned_bucket int,                                -- 3 | 2 | 1 | NULL — last days_remaining value notified (DLG-Q6)
    review_window_days        int,                                -- per-delegation override (DEL-14)
    status                    public.delegation_status NOT NULL DEFAULT 'active',
    record_version            bigint NOT NULL DEFAULT 1,
    created_at                timestamp with time zone NOT NULL DEFAULT now(),
    updated_at                timestamp with time zone NOT NULL DEFAULT now(),
    deleted_at                timestamp with time zone,
    CONSTRAINT delegations_pkey                     PRIMARY KEY (id),
    CONSTRAINT delegations_record_version_check     CHECK (record_version > 0),
    CONSTRAINT chk_review_last_warned_bucket         CHECK (review_last_warned_bucket IS NULL OR review_last_warned_bucket BETWEEN 1 AND 3),
    CONSTRAINT chk_review_window_days                CHECK (review_window_days IS NULL OR review_window_days BETWEEN 1 AND 180),
    CONSTRAINT chk_scope_id CHECK (
        (scope = 'all' AND scope_id IS NULL)
        OR (scope IN ('department', 'tender') AND scope_id IS NOT NULL)
    ),
    CONSTRAINT chk_no_self_delegate  CHECK (delegator_id <> delegate_id),
    CONSTRAINT chk_ends_after_starts CHECK (ends_at IS NULL OR ends_at > starts_at)
);

-- Five partial indexes exactly per LLD §7.2.1, plus idx_delegations_starts_at
-- (DLG-D25, cross-service future-OOO bug fix) for the new activation sweep.
CREATE INDEX idx_delegations_tenant     ON public.delegations (tenant_id)               WHERE deleted_at IS NULL AND status = 'active';
CREATE INDEX idx_delegations_delegator  ON public.delegations (tenant_id, delegator_id) WHERE deleted_at IS NULL AND status = 'active';
CREATE INDEX idx_delegations_delegate   ON public.delegations (tenant_id, delegate_id)  WHERE deleted_at IS NULL AND status = 'active';
CREATE INDEX idx_delegations_ends_at    ON public.delegations (ends_at)                 WHERE deleted_at IS NULL AND status = 'active' AND ends_at IS NOT NULL;
CREATE INDEX idx_delegations_review_due ON public.delegations (review_due_at)           WHERE ends_at IS NULL AND status = 'active';
CREATE INDEX idx_delegations_starts_at  ON public.delegations (starts_at)               WHERE deleted_at IS NULL AND status = 'scheduled';

-- ── delegation_tenant_settings (LLD §7.2.2, DLG-D2) ──────────────────────
-- Relocated from Core's tenants.delegation_max_duration_days /
-- delegation_review_window_days columns. A tenant with no row uses the
-- 90/90 defaults (lazily created on first DLG-7 write).
CREATE TABLE public.delegation_tenant_settings (
    tenant_id          uuid NOT NULL,
    max_duration_days  int NOT NULL DEFAULT 90,
    review_window_days int NOT NULL DEFAULT 90,
    record_version     bigint NOT NULL DEFAULT 1,
    created_at         timestamp with time zone NOT NULL DEFAULT now(),
    updated_at         timestamp with time zone NOT NULL DEFAULT now(),
    CONSTRAINT delegation_tenant_settings_pkey                 PRIMARY KEY (tenant_id),
    CONSTRAINT delegation_tenant_settings_record_version_check CHECK (record_version > 0),
    CONSTRAINT chk_dts_max_duration_days                       CHECK (max_duration_days BETWEEN 1 AND 180),
    CONSTRAINT chk_dts_review_window_days                      CHECK (review_window_days BETWEEN 1 AND 180)
);

-- ── processed_events (LLD §7.2.3) ─────────────────────────────────────────
-- Idempotency ledger for the inbound cascade consumer
-- (consumer ∈ {cascade, offboarding, delegate_disable}). Exempt from RLS
-- (operational). Monthly-pruned (LLD §18.4).
CREATE TABLE public.processed_events (
    event_id     text NOT NULL,
    consumer     text NOT NULL,
    processed_at timestamp with time zone NOT NULL DEFAULT now(),
    CONSTRAINT processed_events_pkey PRIMARY KEY (event_id, consumer)
);

-- Prune path (delegation-cleanup / ProcessedEventsRepository.Prune) filters
-- on processed_at; matching iam-realm-provisioner / iam-org-membership.
CREATE INDEX idx_processed_events_processed_at ON public.processed_events (processed_at);

-- ── rls_violation_log (sampled audit trail) ───────────────────────────────
-- RLS is DISABLED on this table (below) so log_rls_violation() — which
-- fires inside an RLS-check context — cannot recurse into itself.
CREATE TABLE public.rls_violation_log (
    id                bigserial PRIMARY KEY,
    table_name        text NOT NULL,
    row_tenant_id     uuid,
    app_tenant_id     uuid,
    violation_type    text NOT NULL,
    user_id           uuid,
    session_role      text DEFAULT SESSION_USER,
    client_addr       inet DEFAULT inet_client_addr(),
    application_name  text DEFAULT current_setting('application_name', true),
    query_text        text,
    occurred_at       timestamp with time zone NOT NULL DEFAULT now()
);

-- ── touch_row() trigger (LLD §7.5, TRG-1…3) ──────────────────────────────
-- The trigger — not application code — owns both updated_at and
-- record_version; the WHEN guard means a no-op UPDATE does not churn the
-- version (TRG-3).
CREATE OR REPLACE FUNCTION public.touch_row() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at     := now();
    NEW.record_version := OLD.record_version + 1;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_touch_delegations
    BEFORE UPDATE ON public.delegations
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_delegation_tenant_settings
    BEFORE UPDATE ON public.delegation_tenant_settings
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

-- ── Row-Level Security (LLD §7.3) ─────────────────────────────────────────
--
-- Two functions:
--   log_rls_violation — SECURITY DEFINER, 1% sampled INSERT into
--       rls_violation_log. Silently swallows its own errors so a logging
--       failure can never abort the caller's transaction.
--   rls_check_tenant — STABLE STRICT SECURITY DEFINER, returns true when the
--       row's tenant_id matches app.tenant_id GUC. Logs one of two violation
--       types on failure:
--         • missing_or_invalid_guc — no GUC set / malformed UUID
--         • cross_tenant_access    — GUC present but points at a different tenant
--
-- Policies use rls_check_tenant in USING (reads gated + logged) and a plain
-- tenant_id = app_tenant_id() comparison in WITH CHECK (writes gated,
-- LLD §7.3) so a row can never be written into another tenant. ENABLE +
-- FORCE means even the table owner cannot bypass RLS without an explicit
-- BYPASSRLS role.
--
-- rls_violation_log itself has RLS DISABLED — required so log_rls_violation
-- (invoked from an RLS-check context) cannot recurse into itself.

CREATE OR REPLACE FUNCTION public.log_rls_violation(
    p_table_name     text,
    p_row_tenant_id  uuid,
    p_violation_type text
) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = public AS $$
BEGIN
    IF random() > 0.01 THEN RETURN; END IF;
    INSERT INTO rls_violation_log (
        table_name, row_tenant_id, app_tenant_id, violation_type, query_text
    ) VALUES (
        p_table_name, p_row_tenant_id, app_tenant_id(), p_violation_type, current_query()
    );
EXCEPTION WHEN OTHERS THEN
    NULL; -- logging failure must never abort the caller's transaction
END;
$$;

CREATE OR REPLACE FUNCTION public.rls_check_tenant(
    p_tenant_id  uuid,
    p_table_name text
) RETURNS boolean
    LANGUAGE plpgsql STABLE STRICT SECURITY DEFINER
    SET search_path = public AS $$
DECLARE
    v_app uuid;
BEGIN
    v_app := app_tenant_id();
    IF v_app IS NULL THEN
        PERFORM log_rls_violation(p_table_name, p_tenant_id, 'missing_or_invalid_guc');
        RETURN false;
    END IF;
    IF p_tenant_id <> v_app THEN
        PERFORM log_rls_violation(p_table_name, p_tenant_id, 'cross_tenant_access');
        RETURN false;
    END IF;
    RETURN true;
END;
$$;

-- ── ENABLE + FORCE RLS on both tenant-scoped tables ───────────────────────
ALTER TABLE public.delegations                 ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.delegations                 FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.delegation_tenant_settings  ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.delegation_tenant_settings  FORCE  ROW LEVEL SECURITY;

-- Explicitly REVOKE from PUBLIC so a mis-provisioned role cannot silently
-- read tenant tables without going through the policy.
REVOKE ALL ON public.delegations, public.delegation_tenant_settings FROM PUBLIC;

-- ── Policies ─────────────────────────────────────────────────────────────
CREATE POLICY delegations_rls ON public.delegations
    USING      (rls_check_tenant(tenant_id, 'delegations'))
    WITH CHECK (tenant_id = app_tenant_id());

CREATE POLICY delegation_tenant_settings_rls ON public.delegation_tenant_settings
    USING      (rls_check_tenant(tenant_id, 'delegation_tenant_settings'))
    WITH CHECK (tenant_id = app_tenant_id());

-- ── rls_violation_log: RLS DISABLED to prevent recursion ─────────────────
ALTER TABLE public.rls_violation_log DISABLE ROW LEVEL SECURITY;
ALTER TABLE public.rls_violation_log NO FORCE ROW LEVEL SECURITY;

-- ── Roles (LLD §7.3/§7.4) ─────────────────────────────────────────────────
-- delegation_app: the runtime role. Never holds BYPASSRLS (RLS-4) — every
-- statement it issues is subject to FORCE ROW LEVEL SECURITY.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'delegation_app') THEN
        -- Dev-only password; production deployments rotate this out of band,
        -- never by editing this checked-in migration.
        CREATE ROLE delegation_app LOGIN PASSWORD 'delegation_app_dev_password' NOBYPASSRLS;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO delegation_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON
    public.delegations,
    public.delegation_tenant_settings,
    public.processed_events
    TO delegation_app;
GRANT EXECUTE ON FUNCTION public.app_tenant_id()                     TO delegation_app;
GRANT EXECUTE ON FUNCTION public.log_rls_violation(text, uuid, text) TO delegation_app;
GRANT EXECUTE ON FUNCTION public.rls_check_tenant(uuid, text)        TO delegation_app;

-- outbox_events is created by platform-events' own migration (outbox.ApplySchema),
-- which postgres.Migrate runs before this one — so the table already exists by
-- the time this GRANT runs. Without it the outbox runner (SELECT/UPDATE) and
-- the application's INSERT both fail with "permission denied for table
-- outbox_events" (SQLSTATE 42501).
GRANT SELECT, INSERT, UPDATE, DELETE ON public.outbox_events TO delegation_app;

-- ─────────────────────────────────────────────────────────────────────────
-- outbox_events customization — platform-events creates outbox_events with
-- a JSONB payload column (via outbox.ApplySchema, which postgres.Migrate
-- runs BEFORE this domain migration). pgx encodes []byte as bytea hex in
-- PgBouncer SimpleProtocol mode, which is invalid for jsonb; text accepts
-- the raw bytes as-is, and the outbox runner reads the value back via JSON
-- unmarshal, which works identically with text.
-- Matching iam-realm-provisioner / iam-org-membership.
--
-- This service is not yet deployed, so the ALTER is folded into the single
-- 000001_schema migration (same "runs once, against an empty DB" invariant
-- as the siblings). A USING-clause ALTER COLUMN TYPE takes an ACCESS
-- EXCLUSIVE lock and rewrites the table; on a fresh empty outbox that is
-- instant. Do not reuse this file as a template for re-running against a
-- populated outbox_events.
-- ─────────────────────────────────────────────────────────────────────────
ALTER TABLE public.outbox_events ALTER COLUMN payload TYPE text USING payload::text;

-- pgx SimpleProtocol encodes []byte as bytea hex (\x...) even for text
-- columns. This trigger decodes it back to UTF-8 text on every INSERT so
-- the outbox runner can JSON-unmarshal the payload without errors.
CREATE OR REPLACE FUNCTION public.outbox_normalize_payload()
RETURNS trigger AS $$
BEGIN
  IF left(NEW.payload, 2) = '\x' THEN
    NEW.payload = convert_from(decode(substring(NEW.payload FROM 3), 'hex'), 'UTF8');
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_outbox_normalize_payload
BEFORE INSERT ON public.outbox_events
FOR EACH ROW EXECUTE FUNCTION public.outbox_normalize_payload();

GRANT EXECUTE ON FUNCTION public.outbox_normalize_payload() TO delegation_app;

-- delegation_migrator: applies schema migrations and backs the reconciler
-- jobs / cascade consumer's cross-tenant reads (LLD §7.4). Holds BYPASSRLS.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'delegation_migrator') THEN
        CREATE ROLE delegation_migrator LOGIN PASSWORD 'delegation_migrator_dev_password';
    END IF;
END
$$;

ALTER ROLE delegation_migrator BYPASSRLS;
GRANT ALL PRIVILEGES ON SCHEMA public TO delegation_migrator;
GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO delegation_migrator;
GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO delegation_migrator;
