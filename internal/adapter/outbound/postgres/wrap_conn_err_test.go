package postgres

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrapConnErr_NilPassesThroughUnchanged(t *testing.T) {
	assert.NoError(t, wrapConnErr(nil))
}

func TestWrapConnErr_DomainErrorPassesThroughUnchanged(t *testing.T) {
	derr := domain.NewError(domain.ErrDelegationNotFound, "gone")
	got := wrapConnErr(derr)
	assert.Same(t, derr, got)
}

func TestWrapConnErr_ConstraintPgErrorPassesThroughUnchanged(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505"}
	got := wrapConnErr(pgErr)
	assert.Same(t, pgErr, got)
}

func TestWrapConnErr_AvailabilitySQLStateMapsToDBUnavailable(t *testing.T) {
	for _, code := range []string{"08006", "53300", "57P01", "58030"} {
		t.Run(code, func(t *testing.T) {
			got := wrapConnErr(&pgconn.PgError{Code: code})
			var de *domain.Error
			require.True(t, errors.As(got, &de))
			assert.Equal(t, domain.ErrDBUnavailable.Error(), de.Code)
		})
	}
}

func TestWrapConnErr_ErrNoRowsPassesThroughUnchanged(t *testing.T) {
	got := wrapConnErr(pgx.ErrNoRows)
	assert.Same(t, pgx.ErrNoRows, got)
}

func TestWrapConnErr_ContextCanceledPassesThroughUnchanged(t *testing.T) {
	got := wrapConnErr(context.Canceled)
	assert.Same(t, context.Canceled, got)
}

func TestWrapConnErr_UnrecognizedGenericErrorPassesThroughUnchanged(t *testing.T) {
	businessErr := errors.New("boom: simulated business-rule failure")
	got := wrapConnErr(businessErr)
	assert.Same(t, businessErr, got, "must not reclassify a callback's own error as db_unavailable")
}

func TestWrapConnErr_ClosedPoolMapsToDBUnavailable(t *testing.T) {
	got := wrapConnErr(puddle.ErrClosedPool)
	var de *domain.Error
	require.True(t, errors.As(got, &de))
	assert.Equal(t, domain.ErrDBUnavailable.Error(), de.Code)
}

func TestWrapConnErr_DeadlineExceededPassesThroughUnchanged(t *testing.T) {
	got := wrapConnErr(context.DeadlineExceeded)
	assert.Equal(t, context.DeadlineExceeded, got)
	assert.True(t, errors.Is(got, context.DeadlineExceeded))
}

func TestWrapConnErr_NetworkErrorMapsToDBUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "eof", err: io.EOF},
		{name: "unexpected_eof", err: io.ErrUnexpectedEOF},
		{name: "op_error", err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}},
		{name: "conn_refused_text", err: errors.New("dial tcp: connection refused")},
		{name: "broken_pipe_text", err: errors.New("write: broken pipe")},
		{name: "syscall_reset", err: syscall.ECONNRESET},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapConnErr(tc.err)
			var de *domain.Error
			require.True(t, errors.As(got, &de), "got %v", got)
			assert.Equal(t, domain.ErrDBUnavailable.Error(), de.Code)
		})
	}
}
