package provider

import (
	"context"
	"errors"
	"strings"
)

type FailureClass string

const (
	FailureCanceled               FailureClass = "canceled"
	FailureAuthRequired           FailureClass = "auth_required"
	FailureRateLimited            FailureClass = "rate_limited"
	FailureTransientPreSideEffect FailureClass = "transient_pre_side_effect"
	FailureAmbiguousExternal      FailureClass = "reconciliation_required"
	FailurePermanent              FailureClass = "permanent"
)

type Failure struct {
	Class     FailureClass
	RetrySafe bool
	Reconcile bool
	Message   string
}

func Classify(err error, sideEffectStarted bool) Failure {
	if err == nil {
		return Failure{}
	}
	message := strings.ToLower(err.Error())
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Failure{Class: FailureCanceled, Message: "provider operation canceled"}
	}
	if strings.Contains(message, "auth") || strings.Contains(message, "login") || strings.Contains(message, "unauthorized") {
		return Failure{Class: FailureAuthRequired, Message: "provider authentication required"}
	}
	if strings.Contains(message, "rate") || strings.Contains(message, "429") || strings.Contains(message, "quota") {
		return Failure{Class: FailureRateLimited, RetrySafe: !sideEffectStarted, Reconcile: sideEffectStarted, Message: "provider rate limited"}
	}
	if sideEffectStarted {
		return Failure{Class: FailureAmbiguousExternal, Reconcile: true, Message: "external side-effect outcome is unknown"}
	}
	if temporary, ok := err.(interface{ Temporary() bool }); ok && temporary.Temporary() {
		return Failure{Class: FailureTransientPreSideEffect, RetrySafe: true, Message: "transient provider failure before side effect"}
	}
	return Failure{Class: FailurePermanent, Message: "provider operation failed"}
}
