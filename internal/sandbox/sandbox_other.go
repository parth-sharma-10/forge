//go:build !linux

package sandbox

import "context"

// Run reports that the sandbox is unavailable. The isolation is built from
// Linux-only primitives; there is no partial equivalent worth pretending to.
func Run(context.Context, Spec) error { return ErrUnsupported }

// Preflight reports that the sandbox is unavailable, as Run does.
func Preflight(context.Context, Spec) error { return ErrUnsupported }

// MaybeChild is a no-op: nothing on this platform re-executes itself.
func MaybeChild() {}
