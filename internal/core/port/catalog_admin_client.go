package port

import (
	"context"

	"github.com/google/uuid"
)

// CatalogAdminClient validates a scope="department" delegation's scope_id
// against the Catalog / Admin Config Service's global department catalog
// (GAP-020 / DLG-D13). Parallel in shape to TenderScopeClient — both exist
// to replace FK checks lost when delegation was extracted from Core's
// database; neither is a hard startup requirement (nil is valid and degrades
// to presence-only scope_id checking).
type CatalogAdminClient interface {
	// DepartmentActive reports whether departmentID names a department in
	// the global catalog and whether it is currently active (is_active=true).
	//
	// err is non-nil ONLY when the check itself could not be performed
	// (network timeout / 5xx) — it is never used to express "not active".
	// On transport failure err wraps ErrDependencyUnavailable so callers
	// can fail closed (503 catalog_admin_unavailable) via errors.Is.
	//
	// exists==false or active==false with err==nil is a business rejection
	// (422 invalid_scope_id): the department either does not exist in the
	// global catalog or has been retired by an operator.
	DepartmentActive(ctx context.Context, departmentID uuid.UUID) (exists bool, active bool, err error)
}
