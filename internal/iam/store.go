package iam

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"go.etcd.io/bbolt"
	"go.uber.org/zap"
)

var (
	bucketUsers        = []byte("iam_users")
	bucketGroups       = []byte("iam_groups")
	bucketPolicies     = []byte("iam_policies")
	bucketRoles        = []byte("iam_roles")
	bucketBucketPolicy = []byte("iam_bucket_policies")
	bucketTempCreds    = []byte("iam_temp_credentials")
)

// IAMStore provides BoltDB-backed persistence for IAM data
type IAMStore struct {
	mu   sync.RWMutex
	db   *bbolt.DB
	path string
	scp  *PolicyDocument // Service Control Policy (organization-level)
}

// NewIAMStore creates a new IAM store backed by BoltDB
func NewIAMStore(path string) (*IAMStore, error) {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open IAM database: %w", err)
	}

	// Create all buckets
	err = db.Update(func(tx *bbolt.Tx) error {
		buckets := [][]byte{bucketUsers, bucketGroups, bucketPolicies, bucketRoles, bucketBucketPolicy, bucketTempCreds}
		for _, b := range buckets {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return fmt.Errorf("failed to create bucket %s: %w", string(b), err)
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	store := &IAMStore{
		db:   db,
		path: path,
	}

	zap.L().Info("IAM store initialized", zap.String("path", path))
	return store, nil
}

// Close closes the store
func (s *IAMStore) Close() error {
	return s.db.Close()
}

// --- User operations ---

func (s *IAMStore) PutUser(user *IAMUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(user)
	if err != nil {
		return fmt.Errorf("failed to marshal user: %w", err)
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		return b.Put([]byte(user.Name), data)
	})
}

func (s *IAMStore) GetUser(name string) (*IAMUser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var user *IAMUser
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		data := b.Get([]byte(name))
		if data == nil {
			return fmt.Errorf("user %s not found", name)
		}
		return json.Unmarshal(data, &user)
	})
	return user, err
}

func (s *IAMStore) DeleteUser(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		return b.Delete([]byte(name))
	})
}

func (s *IAMStore) ListUsers() ([]*IAMUser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var users []*IAMUser
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		return b.ForEach(func(k, v []byte) error {
			var user IAMUser
			if err := json.Unmarshal(v, &user); err != nil {
				return nil // skip malformed
			}
			users = append(users, &user)
			return nil
		})
	})
	return users, err
}

func (s *IAMStore) GetUserByAccessKey(accessKeyID string) (*IAMUser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var found *IAMUser
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		return b.ForEach(func(k, v []byte) error {
			var user IAMUser
			if err := json.Unmarshal(v, &user); err != nil {
				return nil
			}
			for _, ak := range user.AccessKeys {
				if ak.AccessKeyID == accessKeyID && ak.Status == AccessKeyActive {
					found = &user
					return fmt.Errorf("found") // break early
				}
			}
			return nil
		})
	})
	if found != nil {
		return found, nil
	}
	return nil, err
}

// --- Group operations ---

func (s *IAMStore) PutGroup(group *IAMGroup) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(group)
	if err != nil {
		return fmt.Errorf("failed to marshal group: %w", err)
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketGroups)
		return b.Put([]byte(group.Name), data)
	})
}

func (s *IAMStore) GetGroup(name string) (*IAMGroup, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var group *IAMGroup
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketGroups)
		data := b.Get([]byte(name))
		if data == nil {
			return fmt.Errorf("group %s not found", name)
		}
		return json.Unmarshal(data, &group)
	})
	return group, err
}

func (s *IAMStore) DeleteGroup(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketGroups)
		return b.Delete([]byte(name))
	})
}

func (s *IAMStore) ListGroups() ([]*IAMGroup, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var groups []*IAMGroup
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketGroups)
		return b.ForEach(func(k, v []byte) error {
			var group IAMGroup
			if err := json.Unmarshal(v, &group); err != nil {
				return nil
			}
			groups = append(groups, &group)
			return nil
		})
	})
	return groups, err
}

// --- Policy operations ---

func (s *IAMStore) PutPolicy(policy *IAMPolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("failed to marshal policy: %w", err)
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketPolicies)
		return b.Put([]byte(policy.Name), data)
	})
}

func (s *IAMStore) GetPolicy(name string) (*IAMPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var policy *IAMPolicy
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketPolicies)
		data := b.Get([]byte(name))
		if data == nil {
			return fmt.Errorf("policy %s not found", name)
		}
		return json.Unmarshal(data, &policy)
	})
	return policy, err
}

func (s *IAMStore) DeletePolicy(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketPolicies)
		return b.Delete([]byte(name))
	})
}

func (s *IAMStore) ListPolicies() ([]*IAMPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var policies []*IAMPolicy
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketPolicies)
		return b.ForEach(func(k, v []byte) error {
			var policy IAMPolicy
			if err := json.Unmarshal(v, &policy); err != nil {
				return nil
			}
			policies = append(policies, &policy)
			return nil
		})
	})
	return policies, err
}

// --- Role operations ---

func (s *IAMStore) PutRole(role *IAMRole) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(role)
	if err != nil {
		return fmt.Errorf("failed to marshal role: %w", err)
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketRoles)
		return b.Put([]byte(role.Name), data)
	})
}

func (s *IAMStore) GetRole(name string) (*IAMRole, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var role *IAMRole
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketRoles)
		data := b.Get([]byte(name))
		if data == nil {
			return fmt.Errorf("role %s not found", name)
		}
		return json.Unmarshal(data, &role)
	})
	return role, err
}

func (s *IAMStore) DeleteRole(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketRoles)
		return b.Delete([]byte(name))
	})
}

func (s *IAMStore) ListRoles() ([]*IAMRole, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var roles []*IAMRole
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketRoles)
		return b.ForEach(func(k, v []byte) error {
			var role IAMRole
			if err := json.Unmarshal(v, &role); err != nil {
				return nil
			}
			roles = append(roles, &role)
			return nil
		})
	})
	return roles, err
}

// --- Bucket Policy operations ---

func (s *IAMStore) PutBucketPolicy(bp *BucketPolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(bp)
	if err != nil {
		return fmt.Errorf("failed to marshal bucket policy: %w", err)
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketBucketPolicy)
		return b.Put([]byte(bp.Bucket), data)
	})
}

func (s *IAMStore) GetBucketPolicy(bucket string) (*BucketPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var bp *BucketPolicy
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketBucketPolicy)
		data := b.Get([]byte(bucket))
		if data == nil {
			return fmt.Errorf("bucket policy for %s not found", bucket)
		}
		return json.Unmarshal(data, &bp)
	})
	return bp, err
}

func (s *IAMStore) DeleteBucketPolicy(bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketBucketPolicy)
		return b.Delete([]byte(bucket))
	})
}

// --- Temporary Credential operations (for STS) ---

func (s *IAMStore) PutTempCredential(cred *TemporaryCredential) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("failed to marshal temp credential: %w", err)
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketTempCreds)
		return b.Put([]byte(cred.AccessKeyID), data)
	})
}

func (s *IAMStore) GetTempCredential(accessKeyID string) (*TemporaryCredential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var cred *TemporaryCredential
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketTempCreds)
		data := b.Get([]byte(accessKeyID))
		if data == nil {
			return fmt.Errorf("temp credential %s not found", accessKeyID)
		}
		if err := json.Unmarshal(data, &cred); err != nil {
			return err
		}
		// Check expiration
		if time.Now().After(cred.Expiration) {
			return fmt.Errorf("temp credential expired")
		}
		return nil
	})
	return cred, err
}

func (s *IAMStore) DeleteTempCredential(accessKeyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketTempCreds)
		return b.Delete([]byte(accessKeyID))
	})
}

// CleanupExpiredTempCreds removes expired temporary credentials
func (s *IAMStore) CleanupExpiredTempCreds() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	now := time.Now()

	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketTempCreds)
		return b.ForEach(func(k, v []byte) error {
			var cred TemporaryCredential
			if err := json.Unmarshal(v, &cred); err != nil {
				return nil
			}
			if now.After(cred.Expiration) {
				b.Delete(k)
				count++
			}
			return nil
		})
	})
	return count, err
}
