package iam

import "fmt"

// SCP (Service Control Policy) support for IAM.
//
// An SCP is an organization-level policy that constrains all IAM decisions.
// It acts as the first gate in the policy evaluation chain:
//  1. Check SCP for explicit Deny → Deny (overrides everything)
//  2. Check SCP for Allow → if SCP doesn't allow, deny
//  3. Continue with normal evaluation (identity policies, boundaries, etc.)
//
// SCPs use the same PolicyDocument format as other IAM policies.

// SetSCP sets the organization's Service Control Policy.
func (s *IAMStore) SetSCP(doc *PolicyDocument) error {
	if doc == nil {
		return fmt.Errorf("SCP document cannot be nil")
	}
	if len(doc.Statement) == 0 {
		return fmt.Errorf("SCP document must contain at least one statement")
	}
	s.scp = doc
	return nil
}

// GetSCP returns the current Service Control Policy.
func (s *IAMStore) GetSCP() *PolicyDocument {
	return s.scp
}

// RemoveSCP removes the Service Control Policy (no SCP constraint).
func (s *IAMStore) RemoveSCP() {
	s.scp = nil
}
