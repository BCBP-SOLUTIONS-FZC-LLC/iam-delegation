package domain

import "testing"

func TestErr_Error_ReturnsCodeColonMessage(t *testing.T) {
	e := &Error{Code: "validation_error", Message: "field is required"}
	want := "validation_error: field is required"
	if got := e.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestErr_Error_ViaNewError(t *testing.T) {
	e := NewError(ErrDelegationNotFound, "record gone")
	want := "delegation_not_found: record gone"
	if got := e.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}
