// Package werr carries the world's stable, machine-readable error codes.
//
// Agents branch on these strings, so they are part of the public contract:
// renaming one breaks every agent in the world. Add codes freely; change them
// almost never.
//
// The codes live in the kernel rather than in the API layer because domain
// services need to produce them without importing HTTP concerns.
package werr

import "errors"

// Code is a stable error identifier (PHASE-1-SPEC §4).
type Code string

const (
	InsufficientFunds   Code = "insufficient_funds"
	NotFound            Code = "not_found"
	Forbidden           Code = "forbidden"
	InvalidParams       Code = "invalid_params"
	CooldownActive      Code = "cooldown_active"
	CapacityFull        Code = "capacity_full"
	Incapacitated       Code = "incapacitated"
	RateLimited         Code = "rate_limited"
	IdempotencyConflict Code = "idempotency_conflict"

	// NameTaken is distinct from InvalidParams because the caller's remedy is
	// different: pick another name rather than fix the request.
	NameTaken Code = "name_taken"

	// Internal is never the agent's fault and never carries detail — an error
	// message is an information leak to a caller assumed hostile (invariant #6).
	Internal Code = "internal"
)

// Error is a coded, agent-safe error. Message is shown to the caller, so it
// must never contain internal detail.
type Error struct {
	Code    Code
	Message string
	// Cause is logged, never serialised.
	Cause error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return string(e.Code) + ": " + e.Message + ": " + e.Cause.Error()
	}
	return string(e.Code) + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func Wrap(code Code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

// CodeOf reports the code carried by err, or Internal if it carries none.
// An error without a code is a bug we have not classified yet, and the caller
// should learn nothing about it.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return Internal
}

// MessageOf returns the agent-safe message for err, or a generic one.
func MessageOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Message
	}
	return "internal error"
}
