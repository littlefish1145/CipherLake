package iam

import "fmt"

// Permission Boundary support for IAM.
//
// A permission boundary is an advanced policy type that sets the maximum
// permissions that an identity-based policy can grant to an IAM entity
// (user or role). Effective permission = Identity Policy ∩ Permission Boundary.
//
// If a permission boundary denies an action, the result is deny even if
// the identity policy allows it.

// SetPermissionBoundary sets the permission boundary for a user.
// The boundary is a policy name reference that will be looked up during evaluation.
func (s *IAMService) SetPermissionBoundary(userName, policyName string) error {
	user, err := s.store.GetUser(userName)
	if err != nil {
		return fmt.Errorf("user %s not found", userName)
	}

	// Verify the boundary policy exists
	if _, err := s.store.GetPolicy(policyName); err != nil {
		return fmt.Errorf("boundary policy %s not found", policyName)
	}

	user.PermissionBoundary = policyName
	return s.store.PutUser(user)
}

// RemovePermissionBoundary removes the permission boundary from a user.
func (s *IAMService) RemovePermissionBoundary(userName string) error {
	user, err := s.store.GetUser(userName)
	if err != nil {
		return fmt.Errorf("user %s not found", userName)
	}

	user.PermissionBoundary = ""
	return s.store.PutUser(user)
}
