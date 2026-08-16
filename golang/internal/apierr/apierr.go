// Package apierr defines error types shared by internal/k8shealth and
// internal/netpolicy so internal/api can map either package's errors to the
// right HTTP status code in one place, rather than duplicating the mapping
// (or the types) per package.
package apierr

// ValidationError indicates the request itself can't be fulfilled as given
// - not a malformed body (that's caught before either package is called),
// but something like a namespace that doesn't exist. Callers map it to 400.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// NotFoundError indicates the referenced object doesn't exist. Callers map
// it to 404.
type NotFoundError struct{ Msg string }

func (e *NotFoundError) Error() string { return e.Msg }

// PermissionError indicates this service's own ServiceAccount lacks the
// Kubernetes RBAC to perform the action - distinct from a caller lacking
// permission, which internal/authn rejects earlier before this service's
// own credentials are ever used. Expected in namespace-scoped RBAC mode if
// a request names a namespace outside the configured set. Callers map it
// to 403.
type PermissionError struct{ Msg string }

func (e *PermissionError) Error() string { return e.Msg }
