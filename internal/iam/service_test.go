package iam

import (
	"path/filepath"
	"strings"
	"testing"
)

func newTestIAMService(t *testing.T) (*IAMService, func()) {
	t.Helper()

	tmpDir := t.TempDir()
	store, err := NewIAMStore(filepath.Join(tmpDir, "iam.db"))
	if err != nil {
		t.Fatalf("NewIAMStore() error = %v", err)
	}
	masterKey, err := NewMasterKey(filepath.Join(tmpDir, "master.key"))
	if err != nil {
		store.Close()
		t.Fatalf("NewMasterKey() error = %v", err)
	}

	return NewIAMService(store, masterKey), func() {
		masterKey.Zero()
		if err := store.Close(); err != nil {
			t.Fatalf("store.Close() error = %v", err)
		}
	}
}

func testPolicyDocument() PolicyDocument {
	return PolicyDocument{
		Version: PolicyVersion,
		Statement: []Statement{
			{
				Effect:   EffectAllow,
				Action:   StringOrSlice{"s3:GetObject"},
				Resource: StringOrSlice{"*"},
			},
		},
	}
}

func TestDeletePolicyRejectsAttachedReferences(t *testing.T) {
	tests := []struct {
		name       string
		attach     func(t *testing.T, svc *IAMService, policyName string)
		wantErrSub string
	}{
		{
			name: "user attached policy",
			attach: func(t *testing.T, svc *IAMService, policyName string) {
				if _, err := svc.CreateUser("alice", "Alice"); err != nil {
					t.Fatalf("CreateUser() error = %v", err)
				}
				if err := svc.AttachUserPolicy("alice", policyName); err != nil {
					t.Fatalf("AttachUserPolicy() error = %v", err)
				}
			},
			wantErrSub: "attached to user alice",
		},
		{
			name: "user permission boundary",
			attach: func(t *testing.T, svc *IAMService, policyName string) {
				if _, err := svc.CreateUser("alice", "Alice"); err != nil {
					t.Fatalf("CreateUser() error = %v", err)
				}
				user, err := svc.GetUser("alice")
				if err != nil {
					t.Fatalf("GetUser() error = %v", err)
				}
				user.PermissionBoundary = MakePolicyARN(policyName)
				if err := svc.GetStore().PutUser(user); err != nil {
					t.Fatalf("PutUser() error = %v", err)
				}
			},
			wantErrSub: "permission boundary by user alice",
		},
		{
			name: "group attached policy",
			attach: func(t *testing.T, svc *IAMService, policyName string) {
				if _, err := svc.CreateGroup("operators", "Operators"); err != nil {
					t.Fatalf("CreateGroup() error = %v", err)
				}
				if err := svc.AttachGroupPolicy("operators", policyName); err != nil {
					t.Fatalf("AttachGroupPolicy() error = %v", err)
				}
			},
			wantErrSub: "attached to group operators",
		},
		{
			name: "role attached policy",
			attach: func(t *testing.T, svc *IAMService, policyName string) {
				if _, err := svc.CreateRole("ingest", "Ingest role", PolicyDocument{}, 0); err != nil {
					t.Fatalf("CreateRole() error = %v", err)
				}
				if err := svc.AttachRolePolicy("ingest", policyName); err != nil {
					t.Fatalf("AttachRolePolicy() error = %v", err)
				}
			},
			wantErrSub: "attached to role ingest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, cleanup := newTestIAMService(t)
			defer cleanup()

			if _, err := svc.CreatePolicy("ReadOnly", "read only", testPolicyDocument()); err != nil {
				t.Fatalf("CreatePolicy() error = %v", err)
			}
			tt.attach(t, svc, "ReadOnly")

			err := svc.DeletePolicy("ReadOnly")
			if err == nil {
				t.Fatal("DeletePolicy() error = nil, want attachment error")
			}
			if !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("DeletePolicy() error = %q, want substring %q", err.Error(), tt.wantErrSub)
			}
			if _, err := svc.GetStore().GetPolicy("ReadOnly"); err != nil {
				t.Fatalf("policy should remain after rejected delete: %v", err)
			}
		})
	}
}

func TestDeletePolicyAllowsDetachedPolicy(t *testing.T) {
	svc, cleanup := newTestIAMService(t)
	defer cleanup()

	if _, err := svc.CreatePolicy("ReadOnly", "read only", testPolicyDocument()); err != nil {
		t.Fatalf("CreatePolicy() error = %v", err)
	}
	if _, err := svc.CreateUser("alice", "Alice"); err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	if err := svc.AttachUserPolicy("alice", "ReadOnly"); err != nil {
		t.Fatalf("AttachUserPolicy() error = %v", err)
	}
	if err := svc.DetachUserPolicy("alice", "ReadOnly"); err != nil {
		t.Fatalf("DetachUserPolicy() error = %v", err)
	}

	if err := svc.DeletePolicy("ReadOnly"); err != nil {
		t.Fatalf("DeletePolicy() error = %v", err)
	}
	if _, err := svc.GetStore().GetPolicy("ReadOnly"); err == nil {
		t.Fatal("policy still exists after delete")
	}
}
