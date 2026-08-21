package domain

import (
	"time"

	"github.com/google/uuid"
)

// DelegationTenantSettings is the per-tenant delegation policy, relocated
// from Core's tenants.delegation_max_duration_days /
// delegation_review_window_days columns (LLD §7.2.2, DLG-D2). Read
// in-process at create/extend — no cross-service call. A tenant with no row
// uses the 90/90 defaults (lazily created on first DLG-7 write).
type DelegationTenantSettings struct {
	TenantID         uuid.UUID
	MaxDurationDays  int // [1,180], default 90 (DEL-14)
	ReviewWindowDays int // [1,180], default 90 (DEL-13/14)
	RecordVersion    int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// DefaultPolicyDays are the fallback bounds when a tenant has no
// delegation_tenant_settings row (LLD §15 policyDefaults).
const (
	DefaultMaxDurationDays  = 90
	DefaultReviewWindowDays = 90

	// PolicyDaysMin/Max bound both DLG-7 fields and DLG-4 extend_days (LLD §13.3).
	PolicyDaysMin = 1
	PolicyDaysMax = 180
)

// DefaultDelegationTenantSettings returns the 90/90 default policy for a
// tenant with no persisted row.
func DefaultDelegationTenantSettings(tenantID uuid.UUID) DelegationTenantSettings {
	return DelegationTenantSettings{
		TenantID:         tenantID,
		MaxDurationDays:  DefaultMaxDurationDays,
		ReviewWindowDays: DefaultReviewWindowDays,
	}
}
