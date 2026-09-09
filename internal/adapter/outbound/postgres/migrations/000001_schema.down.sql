-- Reverses 000001_schema.up.sql in full, in reverse dependency order.

-- ── outbox_events customization (reverse first — added last in up.sql) ──
DROP TRIGGER IF EXISTS trg_outbox_normalize_payload ON public.outbox_events;
DROP FUNCTION IF EXISTS public.outbox_normalize_payload();
ALTER TABLE public.outbox_events ALTER COLUMN payload TYPE jsonb USING payload::jsonb;

-- ── Roles ─────────────────────────────────────────────────────────────────
REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM delegation_migrator;
REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM delegation_migrator;
REVOKE ALL PRIVILEGES ON SCHEMA public FROM delegation_migrator;
DROP ROLE IF EXISTS delegation_migrator;

REVOKE SELECT, INSERT, UPDATE, DELETE ON public.outbox_events FROM delegation_app;
REVOKE EXECUTE ON FUNCTION public.rls_check_tenant(uuid, text)        FROM delegation_app;
REVOKE EXECUTE ON FUNCTION public.log_rls_violation(text, uuid, text) FROM delegation_app;
REVOKE EXECUTE ON FUNCTION public.app_tenant_id()                     FROM delegation_app;
REVOKE ALL PRIVILEGES ON
    public.delegations,
    public.delegation_tenant_settings,
    public.processed_events
    FROM delegation_app;
REVOKE USAGE ON SCHEMA public FROM delegation_app;
DROP ROLE IF EXISTS delegation_app;

-- ── RLS policies + disable ────────────────────────────────────────────────
DROP POLICY IF EXISTS delegation_tenant_settings_rls ON public.delegation_tenant_settings;
DROP POLICY IF EXISTS delegations_rls                ON public.delegations;

ALTER TABLE public.delegation_tenant_settings NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.delegation_tenant_settings DISABLE ROW LEVEL SECURITY;
ALTER TABLE public.delegations                NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.delegations                DISABLE ROW LEVEL SECURITY;

DROP FUNCTION IF EXISTS public.rls_check_tenant(uuid, text);
DROP FUNCTION IF EXISTS public.log_rls_violation(text, uuid, text);

-- ── Triggers + touch_row() ────────────────────────────────────────────────
DROP TRIGGER IF EXISTS trg_touch_delegation_tenant_settings ON public.delegation_tenant_settings;
DROP TRIGGER IF EXISTS trg_touch_delegations                ON public.delegations;
DROP FUNCTION IF EXISTS public.touch_row();

-- ── Tables ────────────────────────────────────────────────────────────────
DROP TABLE IF EXISTS public.rls_violation_log;
DROP TABLE IF EXISTS public.processed_events;
DROP TABLE IF EXISTS public.delegation_tenant_settings;
DROP TABLE IF EXISTS public.delegations;

-- ── app_tenant_id() ───────────────────────────────────────────────────────
DROP FUNCTION IF EXISTS public.app_tenant_id();

-- ── Enums ─────────────────────────────────────────────────────────────────
DROP TYPE IF EXISTS public.delegation_status;
DROP TYPE IF EXISTS public.delegation_scope;

-- Extensions are intentionally left in place (pgcrypto may be relied on by
-- other schemas/roles in the same database).
