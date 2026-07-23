package policy

import "errors"

var ErrNativeApprovalDenied = errors.New("policy: native approval mode denied")

func NativeApproval(snapshot Snapshot, requested ApprovalMode) (ApprovalMode, error) {
	if requested == "" {
		requested = ApprovalProviderDefault
	}
	if snapshot.ApprovalMode == ApprovalAskEveryTime && requested != ApprovalAskEveryTime {
		return ApprovalAskEveryTime, nil
	}
	if snapshot.ApprovalMode == ApprovalAutoWithinPolicy && requested == ApprovalAskEveryTime {
		return requested, nil
	}
	if snapshot.ApprovalMode != "" && snapshot.ApprovalMode != requested && snapshot.ApprovalMode != ApprovalProviderDefault {
		return "", ErrNativeApprovalDenied
	}
	return requested, nil
}
