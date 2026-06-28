package iam

import (
	"testing"
	"time"
)

// --- Condition Operator Tests ---

func TestStringEquals(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{Conditions: map[string]string{"env": "production"}}

	tests := []struct {
		operator string
		key      string
		expected interface{}
		want     bool
	}{
		{"StringEquals", "env", "production", true},
		{"StringEquals", "env", "staging", false},
		{"StringEquals", "missing", "value", false},
		{"StringEquals", "env", []interface{}{"staging", "production"}, true},
		{"StringEquals", "env", []interface{}{"staging", "development"}, false},
	}

	for _, tt := range tests {
		got := pe.conditionMatches(tt.operator, tt.key, tt.expected, ctx)
		if got != tt.want {
			t.Errorf("conditionMatches(%q, %q, %v) = %v, want %v", tt.operator, tt.key, tt.expected, got, tt.want)
		}
	}
}

func TestStringNotEquals(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{Conditions: map[string]string{"env": "production"}}

	tests := []struct {
		operator string
		key      string
		expected interface{}
		want     bool
	}{
		{"StringNotEquals", "env", "staging", true},
		{"StringNotEquals", "env", "production", false},
		{"StringNotEquals", "missing", "value", true},
	}

	for _, tt := range tests {
		got := pe.conditionMatches(tt.operator, tt.key, tt.expected, ctx)
		if got != tt.want {
			t.Errorf("conditionMatches(%q, %q, %v) = %v, want %v", tt.operator, tt.key, tt.expected, got, tt.want)
		}
	}
}

func TestStringLike(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{Conditions: map[string]string{"path": "images/photos/vacation.jpg"}}

	tests := []struct {
		operator string
		key      string
		expected interface{}
		want     bool
	}{
		{"StringLike", "path", "images/*", true},
		{"StringLike", "path", "documents/*", false},
		{"StringLike", "path", "images/photos/*", true},
		{"StringLike", "path", "*/vacation.jpg", true},
		{"StringLike", "path", "images/*/vacation.jpg", true},
	}

	for _, tt := range tests {
		got := pe.conditionMatches(tt.operator, tt.key, tt.expected, ctx)
		if got != tt.want {
			t.Errorf("conditionMatches(%q, %q, %v) = %v, want %v", tt.operator, tt.key, tt.expected, got, tt.want)
		}
	}
}

func TestStringNotLike(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{Conditions: map[string]string{"path": "images/photo.jpg"}}

	tests := []struct {
		operator string
		key      string
		expected interface{}
		want     bool
	}{
		{"StringNotLike", "path", "documents/*", true},
		{"StringNotLike", "path", "images/*", false},
	}

	for _, tt := range tests {
		got := pe.conditionMatches(tt.operator, tt.key, tt.expected, ctx)
		if got != tt.want {
			t.Errorf("conditionMatches(%q, %q, %v) = %v, want %v", tt.operator, tt.key, tt.expected, got, tt.want)
		}
	}
}

func TestNumericEquals(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{Conditions: map[string]string{"size": "100"}}

	tests := []struct {
		operator string
		key      string
		expected interface{}
		want     bool
	}{
		{"NumericEquals", "size", "100", true},
		{"NumericEquals", "size", "99", false},
		{"NumericEquals", "size", "100.0", true},
	}

	for _, tt := range tests {
		got := pe.conditionMatches(tt.operator, tt.key, tt.expected, ctx)
		if got != tt.want {
			t.Errorf("conditionMatches(%q, %q, %v) = %v, want %v", tt.operator, tt.key, tt.expected, got, tt.want)
		}
	}
}

func TestNumericNotEquals(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{Conditions: map[string]string{"size": "100"}}

	if !pe.conditionMatches("NumericNotEquals", "size", "99", ctx) {
		t.Error("NumericNotEquals should return true for different values")
	}
	if pe.conditionMatches("NumericNotEquals", "size", "100", ctx) {
		t.Error("NumericNotEquals should return false for equal values")
	}
}

func TestNumericLessThan(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{Conditions: map[string]string{"size": "50"}}

	tests := []struct {
		operator string
		key      string
		expected interface{}
		want     bool
	}{
		{"NumericLessThan", "size", "100", true},
		{"NumericLessThan", "size", "50", false},
		{"NumericLessThan", "size", "25", false},
	}

	for _, tt := range tests {
		got := pe.conditionMatches(tt.operator, tt.key, tt.expected, ctx)
		if got != tt.want {
			t.Errorf("conditionMatches(%q, %q, %v) = %v, want %v", tt.operator, tt.key, tt.expected, got, tt.want)
		}
	}
}

func TestNumericGreaterThan(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{Conditions: map[string]string{"size": "100"}}

	tests := []struct {
		operator string
		key      string
		expected interface{}
		want     bool
	}{
		{"NumericGreaterThan", "size", "50", true},
		{"NumericGreaterThan", "size", "100", false},
		{"NumericGreaterThan", "size", "200", false},
	}

	for _, tt := range tests {
		got := pe.conditionMatches(tt.operator, tt.key, tt.expected, ctx)
		if got != tt.want {
			t.Errorf("conditionMatches(%q, %q, %v) = %v, want %v", tt.operator, tt.key, tt.expected, got, tt.want)
		}
	}
}

func TestDateLessThan(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{Time: time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)}

	if !pe.conditionMatches("DateLessThan", "aws:CurrentTime", "2025-01-01T00:00:00Z", ctx) {
		t.Error("DateLessThan should return true when current time is before expected")
	}
	if pe.conditionMatches("DateLessThan", "aws:CurrentTime", "2023-01-01T00:00:00Z", ctx) {
		t.Error("DateLessThan should return false when current time is after expected")
	}
}

func TestDateGreaterThan(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{Time: time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)}

	if !pe.conditionMatches("DateGreaterThan", "aws:CurrentTime", "2023-01-01T00:00:00Z", ctx) {
		t.Error("DateGreaterThan should return true when current time is after expected")
	}
	if pe.conditionMatches("DateGreaterThan", "aws:CurrentTime", "2025-01-01T00:00:00Z", ctx) {
		t.Error("DateGreaterThan should return false when current time is before expected")
	}
}

func TestIpAddress(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{SourceIP: "192.168.1.100"}

	tests := []struct {
		operator string
		key      string
		expected interface{}
		want     bool
	}{
		{"IpAddress", "aws:SourceIp", "192.168.1.0/24", true},
		{"IpAddress", "aws:SourceIp", "10.0.0.0/8", false},
		{"IpAddress", "aws:SourceIp", "192.168.1.100", true},
		{"IpAddress", "aws:SourceIp", "*", true},
	}

	for _, tt := range tests {
		got := pe.conditionMatches(tt.operator, tt.key, tt.expected, ctx)
		if got != tt.want {
			t.Errorf("conditionMatches(%q, %q, %v) = %v, want %v", tt.operator, tt.key, tt.expected, got, tt.want)
		}
	}
}

func TestNotIpAddress(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{SourceIP: "192.168.1.100"}

	if !pe.conditionMatches("NotIpAddress", "aws:SourceIp", "10.0.0.0/8", ctx) {
		t.Error("NotIpAddress should return true when IP is NOT in range")
	}
	if pe.conditionMatches("NotIpAddress", "aws:SourceIp", "192.168.1.0/24", ctx) {
		t.Error("NotIpAddress should return false when IP IS in range")
	}
}

func TestIpAddressIPv6(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{SourceIP: "2001:db8::1"}

	if !pe.conditionMatches("IpAddress", "aws:SourceIp", "2001:db8::/32", ctx) {
		t.Error("IpAddress should support IPv6 CIDR matching")
	}
	if pe.conditionMatches("IpAddress", "aws:SourceIp", "2001:db9::/32", ctx) {
		t.Error("IpAddress should not match different IPv6 CIDR")
	}
}

func TestBool(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{Conditions: map[string]string{"ssl": "true"}}

	if !pe.conditionMatches("Bool", "ssl", "true", ctx) {
		t.Error("Bool should match true")
	}
	if pe.conditionMatches("Bool", "ssl", "false", ctx) {
		t.Error("Bool should not match false")
	}
}

func TestArnEquals(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{Conditions: map[string]string{"resource": "arn:nexus:s3:::mybucket/mykey"}}

	tests := []struct {
		operator string
		key      string
		expected interface{}
		want     bool
	}{
		{"ArnEquals", "resource", "arn:nexus:s3:::mybucket/mykey", true},
		{"ArnEquals", "resource", "arn:nexus:s3:::otherbucket/*", false},
		{"ArnLike", "resource", "arn:nexus:s3:::mybucket/*", true},
		{"ArnLike", "resource", "arn:nexus:s3:::*", true},
	}

	for _, tt := range tests {
		got := pe.conditionMatches(tt.operator, tt.key, tt.expected, ctx)
		if got != tt.want {
			t.Errorf("conditionMatches(%q, %q, %v) = %v, want %v", tt.operator, tt.key, tt.expected, got, tt.want)
		}
	}
}

func TestNull(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{SourceIP: "10.0.0.1"}

	tests := []struct {
		key      string
		expected interface{}
		want     bool
	}{
		{"aws:SourceIp", "false", true},   // key is present, expected false (not null)
		{"aws:UserAgent", "true", true},   // key is absent, expected true (null)
		{"aws:SourceIp", "true", false},   // key is present, expected true (null) - mismatch
		{"aws:UserAgent", "false", false}, // key is absent, expected false (not null) - mismatch
	}

	for _, tt := range tests {
		got := pe.conditionMatches("Null", tt.key, tt.expected, ctx)
		if got != tt.want {
			t.Errorf("Null condition for key %q, expected %v = %v, want %v", tt.key, tt.expected, got, tt.want)
		}
	}
}

// --- Condition Key Resolution Tests ---

func TestResolveConditionKey(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{
		SourceIP:      "10.0.0.1",
		Time:          time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC),
		UserAgent:     "Mozilla/5.0",
		PrincipalType: "User",
		S3Prefix:      "images/",
		S3ACL:         "private",
		ResourceTags:  map[string]string{"Project": "Alpha"},
		PrincipalTags: map[string]string{"Team": "Backend"},
		RequestTags:   map[string]string{"Env": "production"},
		Conditions:    map[string]string{"custom:key": "custom-value"},
	}

	tests := []struct {
		key  string
		want string
	}{
		{"aws:SourceIp", "10.0.0.1"},
		{"aws:sourceip", "10.0.0.1"},
		{"aws:CurrentTime", "2024-06-15T12:00:00Z"},
		{"aws:UserAgent", "Mozilla/5.0"},
		{"aws:PrincipalType", "User"},
		{"s3:prefix", "images/"},
		{"s3:x-amz-acl", "private"},
		{"aws:ResourceTag/Project", "Alpha"},
		{"aws:PrincipalTag/Team", "Backend"},
		{"aws:RequestTag/Env", "production"},
		{"custom:key", "custom-value"},
		{"aws:ResourceTag/Missing", ""},
		{"unknown:key", ""},
	}

	for _, tt := range tests {
		got := pe.resolveConditionKey(tt.key, ctx)
		if got != tt.want {
			t.Errorf("resolveConditionKey(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

// --- Variable Substitution Tests ---

func TestVariableSubstitution(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{
		PrincipalTags: map[string]string{"Project": "Alpha", "Team": "Backend"},
		ResourceTags:  map[string]string{"Env": "production"},
	}

	tests := []struct {
		input string
		want  string
	}{
		{"${aws:PrincipalTag/Project}", "Alpha"},
		{"prefix-${aws:PrincipalTag/Team}-suffix", "prefix-Backend-suffix"},
		{"no-substitution", "no-substitution"},
		{"${aws:ResourceTag/Env}", "production"},
		{"${aws:PrincipalTag/Missing}", ""},
		{"${aws:PrincipalTag/Project}-${aws:PrincipalTag/Team}", "Alpha-Backend"},
	}

	for _, tt := range tests {
		got := pe.resolveVariableSubstitution(tt.input, ctx)
		if got != tt.want {
			t.Errorf("resolveVariableSubstitution(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- ABAC Tag Matching Tests ---

func TestABACMatch(t *testing.T) {
	ctx := &EvalContext{
		ResourceTags:  map[string]string{"Project": "Alpha"},
		PrincipalTags: map[string]string{"Project": "Alpha"},
	}

	if !ABACMatch(ctx, "Project", "Project") {
		t.Error("ABACMatch should return true when resource and principal tags match")
	}

	ctx2 := &EvalContext{
		ResourceTags:  map[string]string{"Project": "Alpha"},
		PrincipalTags: map[string]string{"Project": "Beta"},
	}

	if ABACMatch(ctx2, "Project", "Project") {
		t.Error("ABACMatch should return false when resource and principal tags differ")
	}
}

func TestABACMatchLiteral(t *testing.T) {
	ctx := &EvalContext{
		ResourceTags: map[string]string{"Environment": "production"},
	}

	if !ABACMatchLiteral(ctx, "Environment", "production") {
		t.Error("ABACMatchLiteral should return true when tag matches literal")
	}
	if ABACMatchLiteral(ctx, "Environment", "staging") {
		t.Error("ABACMatchLiteral should return false when tag doesn't match literal")
	}
	if ABACMatchLiteral(ctx, "Missing", "production") {
		t.Error("ABACMatchLiteral should return false when tag key doesn't exist")
	}
}

func TestABACConditionIntegration(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{
		Principal:     "user1",
		Action:        "s3:GetObject",
		Resource:      "arn:nexus:s3:::mybucket/key",
		ResourceTags:  map[string]string{"Project": "Alpha"},
		PrincipalTags: map[string]string{"Project": "Alpha"},
	}

	// Test ABAC with variable substitution: ResourceTag/Project matches PrincipalTag/Project
	conditions := map[string]map[string]interface{}{
		"StringEquals": {
			"aws:ResourceTag/Project": "${aws:PrincipalTag/Project}",
		},
	}

	if !pe.conditionsMatch(conditions, ctx) {
		t.Error("ABAC condition should match when resource tag equals principal tag via variable substitution")
	}

	// Test ABAC with literal value
	conditions2 := map[string]map[string]interface{}{
		"StringEquals": {
			"aws:ResourceTag/Project": "Alpha",
		},
	}

	if !pe.conditionsMatch(conditions2, ctx) {
		t.Error("ABAC condition should match when resource tag equals literal value")
	}
}

// --- Permission Boundary Tests ---

func TestPermissionBoundaryAllows(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{
		Principal: "user1",
		Action:    "s3:GetObject",
		Resource:  "arn:nexus:s3:::mybucket/key",
	}

	// Boundary allows S3 read access
	boundary := PolicyDocument{
		Version: PolicyVersion,
		Statement: []Statement{
			{
				Effect:   EffectAllow,
				Action:   StringOrSlice{"s3:Get*", "s3:List*"},
				Resource: StringOrSlice{"*"},
			},
		},
	}

	result := pe.evaluateBoundary(ctx, boundary)
	if result.Decision != DecisionAllow {
		t.Error("Permission boundary should allow S3 read access")
	}
}

func TestPermissionBoundaryDenies(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{
		Principal: "user1",
		Action:    "s3:DeleteObject",
		Resource:  "arn:nexus:s3:::mybucket/key",
	}

	// Boundary only allows S3 read access
	boundary := PolicyDocument{
		Version: PolicyVersion,
		Statement: []Statement{
			{
				Effect:   EffectAllow,
				Action:   StringOrSlice{"s3:Get*", "s3:List*"},
				Resource: StringOrSlice{"*"},
			},
		},
	}

	result := pe.evaluateBoundary(ctx, boundary)
	if result.Decision != DecisionImplicitDeny {
		t.Error("Permission boundary should not allow S3 delete access")
	}
}

func TestPermissionBoundaryExplicitDeny(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{
		Principal: "user1",
		Action:    "s3:GetObject",
		Resource:  "arn:nexus:s3:::mybucket/key",
	}

	// Boundary explicitly denies S3 access
	boundary := PolicyDocument{
		Version: PolicyVersion,
		Statement: []Statement{
			{
				Effect:   EffectDeny,
				Action:   StringOrSlice{"s3:*"},
				Resource: StringOrSlice{"*"},
			},
		},
	}

	result := pe.evaluateBoundary(ctx, boundary)
	if result.Decision != DecisionDeny {
		t.Error("Permission boundary explicit deny should take effect")
	}
}

// --- SCP Tests ---

func TestSCPAllow(t *testing.T) {
	scp := &PolicyDocument{
		Version: PolicyVersion,
		Statement: []Statement{
			{
				Effect:   EffectAllow,
				Action:   StringOrSlice{"s3:*"},
				Resource: StringOrSlice{"*"},
			},
		},
	}

	store := &IAMStore{scp: scp}
	pe := &PolicyEvaluator{store: store}

	ctx := &EvalContext{
		Principal: "user1",
		Action:    "s3:GetObject",
		Resource:  "arn:nexus:s3:::mybucket/key",
	}

	result := pe.evaluateSCP(ctx)
	if result.Decision != DecisionAllow {
		t.Error("SCP should allow S3 actions")
	}
}

func TestSCPDeny(t *testing.T) {
	scp := &PolicyDocument{
		Version: PolicyVersion,
		Statement: []Statement{
			{
				Effect:   EffectDeny,
				Action:   StringOrSlice{"iam:*"},
				Resource: StringOrSlice{"*"},
			},
			{
				Effect:   EffectAllow,
				Action:   StringOrSlice{"*"},
				Resource: StringOrSlice{"*"},
			},
		},
	}

	store := &IAMStore{scp: scp}
	pe := &PolicyEvaluator{store: store}

	// IAM action should be denied by SCP
	ctx := &EvalContext{
		Principal: "user1",
		Action:    "iam:CreateUser",
		Resource:  "*",
	}

	result := pe.evaluateSCP(ctx)
	if result.Decision != DecisionDeny {
		t.Error("SCP explicit deny should override allow")
	}

	// S3 action should be allowed by SCP
	ctx2 := &EvalContext{
		Principal: "user1",
		Action:    "s3:GetObject",
		Resource:  "arn:nexus:s3:::mybucket/key",
	}

	result2 := pe.evaluateSCP(ctx2)
	if result2.Decision != DecisionAllow {
		t.Error("SCP should allow S3 actions")
	}
}

func TestSCPImplicitDeny(t *testing.T) {
	// SCP that only allows S3 actions
	scp := &PolicyDocument{
		Version: PolicyVersion,
		Statement: []Statement{
			{
				Effect:   EffectAllow,
				Action:   StringOrSlice{"s3:*"},
				Resource: StringOrSlice{"*"},
			},
		},
	}

	store := &IAMStore{scp: scp}
	pe := &PolicyEvaluator{store: store}

	// IAM action should be implicitly denied (not in SCP allow list)
	ctx := &EvalContext{
		Principal: "user1",
		Action:    "iam:CreateUser",
		Resource:  "*",
	}

	result := pe.evaluateSCP(ctx)
	if result.Decision != DecisionImplicitDeny {
		t.Error("SCP should implicitly deny actions not in Allow list")
	}
}

func TestSCPSetGet(t *testing.T) {
	store := &IAMStore{}

	// No SCP initially
	if store.GetSCP() != nil {
		t.Error("SCP should be nil initially")
	}

	// Set SCP
	scp := &PolicyDocument{
		Version: PolicyVersion,
		Statement: []Statement{
			{
				Effect:   EffectAllow,
				Action:   StringOrSlice{"*"},
				Resource: StringOrSlice{"*"},
			},
		},
	}

	if err := store.SetSCP(scp); err != nil {
		t.Fatalf("SetSCP failed: %v", err)
	}

	// Get SCP
	got := store.GetSCP()
	if got == nil {
		t.Fatal("GetSCP should return the set SCP")
	}
	if len(got.Statement) != 1 {
		t.Errorf("SCP should have 1 statement, got %d", len(got.Statement))
	}

	// Remove SCP
	store.RemoveSCP()
	if store.GetSCP() != nil {
		t.Error("SCP should be nil after removal")
	}
}

func TestSCPValidation(t *testing.T) {
	store := &IAMStore{}

	// Nil SCP should fail
	if err := store.SetSCP(nil); err == nil {
		t.Error("SetSCP should reject nil document")
	}

	// Empty SCP should fail
	if err := store.SetSCP(&PolicyDocument{}); err == nil {
		t.Error("SetSCP should reject empty document")
	}
}

// --- Simulate Tests ---

func TestSimulateResponse(t *testing.T) {
	pe := &PolicyEvaluator{store: nil}
	ctx := &EvalContext{
		Principal: "test-user",
		Action:    "s3:GetObject",
		Resource:  "arn:nexus:s3:::bucket/key",
		Time:      time.Now(),
	}

	result := pe.Simulate(ctx)
	if result.Decision != "ImplicitDeny" {
		t.Errorf("Simulate with no policies should return ImplicitDeny, got %s", result.Decision)
	}
}

// --- IP Range Tests ---

func TestIpInRange(t *testing.T) {
	tests := []struct {
		ip   string
		cidr string
		want bool
	}{
		{"192.168.1.1", "192.168.1.0/24", true},
		{"192.168.2.1", "192.168.1.0/24", false},
		{"10.0.0.1", "10.0.0.0/8", true},
		{"172.16.0.1", "172.16.0.0/12", true},
		{"192.168.1.1", "192.168.1.1", true},  // exact match (no CIDR)
		{"192.168.1.2", "192.168.1.1", false}, // exact match fails
		{"1.2.3.4", "*", true},                 // wildcard
		{"invalid", "192.168.1.0/24", false},   // invalid IP
		{"192.168.1.1", "invalid", false},      // invalid CIDR
		{"2001:db8::1", "2001:db8::/32", true}, // IPv6
		{"2001:db9::1", "2001:db8::/32", false},
	}

	for _, tt := range tests {
		got := ipInRange(tt.ip, tt.cidr)
		if got != tt.want {
			t.Errorf("ipInRange(%q, %q) = %v, want %v", tt.ip, tt.cidr, got, tt.want)
		}
	}
}

// --- Numeric Comparison Tests ---

func TestNumericComparisons(t *testing.T) {
	tests := []struct {
		fn       func(string, string) bool
		actual   string
		expected string
		want     bool
	}{
		{numericEquals, "100", "100", true},
		{numericEquals, "100", "100.0", true},
		{numericEquals, "100", "99", false},
		{numericEquals, "3.14", "3.14", true},
		{numericLessThan, "50", "100", true},
		{numericLessThan, "100", "50", false},
		{numericLessThan, "100", "100", false},
		{numericGreaterThan, "100", "50", true},
		{numericGreaterThan, "50", "100", false},
		{numericGreaterThan, "100", "100", false},
		{numericEquals, "invalid", "invalid", true},   // fallback to string comparison
		{numericLessThan, "invalid", "invalid", false}, // fallback to string comparison
	}

	for _, tt := range tests {
		got := tt.fn(tt.actual, tt.expected)
		if got != tt.want {
			t.Errorf("numeric comparison(%q, %q) = %v, want %v", tt.actual, tt.expected, got, tt.want)
		}
	}
}

// --- ARN Match Tests ---

func TestArnMatch(t *testing.T) {
	pe := &PolicyEvaluator{}

	tests := []struct {
		pattern string
		arn     string
		want    bool
	}{
		{"arn:nexus:s3:::mybucket/*", "arn:nexus:s3:::mybucket/key", true},
		{"arn:nexus:s3:::mybucket/*", "arn:nexus:s3:::otherbucket/key", false},
		{"arn:nexus:s3:::*", "arn:nexus:s3:::anybucket/anykey", true},
		{"*", "anything", true},
		{"arn:nexus:s3:::bucket/key", "arn:nexus:s3:::bucket/key", true},
		{"arn:nexus:s3:::bucket/key", "arn:nexus:s3:::bucket/other", false},
	}

	for _, tt := range tests {
		got := pe.arnMatch(tt.pattern, tt.arn)
		if got != tt.want {
			t.Errorf("arnMatch(%q, %q) = %v, want %v", tt.pattern, tt.arn, got, tt.want)
		}
	}
}

// --- Decision String Tests ---

func TestDecisionString(t *testing.T) {
	tests := []struct {
		d    Decision
		want string
	}{
		{DecisionAllow, "Allow"},
		{DecisionDeny, "Deny"},
		{DecisionImplicitDeny, "ImplicitDeny"},
		{Decision(99), "Unknown"},
	}

	for _, tt := range tests {
		got := tt.d.String()
		if got != tt.want {
			t.Errorf("Decision(%d).String() = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// --- Glob Match Tests ---

func TestGlobMatch(t *testing.T) {
	pe := &PolicyEvaluator{}

	tests := []struct {
		pattern string
		str     string
		want    bool
	}{
		{"*", "anything", true},
		{"*", "", true},
		{"a*", "abc", true},
		{"a*", "bcd", false},
		{"*b", "ab", true},
		{"*b", "abc", false},
		{"a*b", "axb", true},
		{"a*b", "axxxxxb", true},
		{"a?b", "axb", true},
		{"a?b", "ab", false},
		{"a?b", "axxb", false},
		{"a*b*c", "axbxc", true},
		{"a*b*c", "axxc", false},
		{"", "", true},
		{"", "a", false},
		{"a", "", false},
	}

	for _, tt := range tests {
		got := pe.globMatch(tt.pattern, tt.str)
		if got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.str, got, tt.want)
		}
	}
}

// --- Tag Resolution Tests ---

func TestResolveResourceTag(t *testing.T) {
	ctx := &EvalContext{
		ResourceTags: map[string]string{"Project": "Alpha", "Env": "production"},
	}

	if ResolveResourceTag(ctx, "Project") != "Alpha" {
		t.Error("ResolveResourceTag should return the correct tag value")
	}
	if ResolveResourceTag(ctx, "Missing") != "" {
		t.Error("ResolveResourceTag should return empty string for missing tag")
	}
	if ResolveResourceTag(nil, "Project") != "" {
		t.Error("ResolveResourceTag should handle nil context")
	}
	if ResolveResourceTag(&EvalContext{}, "Project") != "" {
		t.Error("ResolveResourceTag should handle nil ResourceTags")
	}
}

func TestResolvePrincipalTag(t *testing.T) {
	ctx := &EvalContext{
		PrincipalTags: map[string]string{"Team": "Backend"},
	}

	if ResolvePrincipalTag(ctx, "Team") != "Backend" {
		t.Error("ResolvePrincipalTag should return the correct tag value")
	}
}

func TestResolveRequestTag(t *testing.T) {
	ctx := &EvalContext{
		RequestTags: map[string]string{"Env": "staging"},
	}

	if ResolveRequestTag(ctx, "Env") != "staging" {
		t.Error("ResolveRequestTag should return the correct tag value")
	}
}

// --- Policy Document Parsing Tests ---

func TestParsePolicyDocument(t *testing.T) {
	json := `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Sid": "Statement1",
				"Effect": "Allow",
				"Action": ["s3:GetObject", "s3:PutObject"],
				"Resource": "arn:nexus:s3:::mybucket/*",
				"Condition": {
					"StringEquals": {
						"aws:PrincipalTag/Team": "Backend"
					}
				}
			}
		]
	}`

	doc, err := ParsePolicyDocumentFromString(json)
	if err != nil {
		t.Fatalf("ParsePolicyDocumentFromString failed: %v", err)
	}

	if doc.Version != "2012-10-17" {
		t.Errorf("Version = %q, want %q", doc.Version, "2012-10-17")
	}
	if len(doc.Statement) != 1 {
		t.Fatalf("Statement count = %d, want 1", len(doc.Statement))
	}
	stmt := doc.Statement[0]
	if stmt.Effect != EffectAllow {
		t.Errorf("Effect = %q, want %q", stmt.Effect, EffectAllow)
	}
	if len(stmt.Action) != 2 {
		t.Errorf("Action count = %d, want 2", len(stmt.Action))
	}
	if stmt.Condition == nil {
		t.Error("Condition should not be nil")
	}
}

// --- Full Evaluation Chain Tests ---

func TestEvaluateWithBoundaryAndSCP(t *testing.T) {
	// Set up SCP that allows S3 and IAM
	scp := &PolicyDocument{
		Version: PolicyVersion,
		Statement: []Statement{
			{
				Effect:   EffectAllow,
				Action:   StringOrSlice{"s3:*", "iam:*"},
				Resource: StringOrSlice{"*"},
			},
		},
	}

	store := &IAMStore{scp: scp}
	pe := &PolicyEvaluator{store: store}

	ctx := &EvalContext{
		Principal: "user1",
		Action:    "s3:GetObject",
		Resource:  "arn:nexus:s3:::mybucket/key",
	}

	// SCP allows, but no identity policies → implicit deny
	result := pe.evaluateSCP(ctx)
	if result.Decision != DecisionAllow {
		t.Error("SCP should allow S3 actions")
	}
}

func TestSCPOverridesEverything(t *testing.T) {
	// SCP denies all IAM actions
	scp := &PolicyDocument{
		Version: PolicyVersion,
		Statement: []Statement{
			{
				Effect:   EffectDeny,
				Action:   StringOrSlice{"iam:*"},
				Resource: StringOrSlice{"*"},
			},
			{
				Effect:   EffectAllow,
				Action:   StringOrSlice{"*"},
				Resource: StringOrSlice{"*"},
			},
		},
	}

	store := &IAMStore{scp: scp}
	pe := &PolicyEvaluator{store: store}

	ctx := &EvalContext{
		Principal: "user1",
		Action:    "iam:CreateUser",
		Resource:  "*",
	}

	result := pe.evaluateSCP(ctx)
	if result.Decision != DecisionDeny {
		t.Error("SCP explicit deny should override allow")
	}
}

// --- Condition Key Case Insensitivity Tests ---

func TestConditionKeyCaseInsensitivity(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{SourceIP: "10.0.0.1"}

	// Both should resolve to the same value
	val1 := pe.resolveConditionKey("aws:SourceIp", ctx)
	val2 := pe.resolveConditionKey("aws:sourceip", ctx)

	if val1 != val2 {
		t.Errorf("Condition key resolution should be case-insensitive: %q != %q", val1, val2)
	}
	if val1 != "10.0.0.1" {
		t.Errorf("Expected 10.0.0.1, got %q", val1)
	}
}

// --- Multi-value Condition Tests ---

func TestMultiValueCondition(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{SourceIP: "192.168.1.100"}

	// Multiple CIDR ranges, any match suffices
	conditions := map[string]map[string]interface{}{
		"IpAddress": {
			"aws:SourceIp": []interface{}{"10.0.0.0/8", "192.168.1.0/24", "172.16.0.0/12"},
		},
	}

	if !pe.conditionsMatch(conditions, ctx) {
		t.Error("IpAddress condition should match when IP is in any of the CIDR ranges")
	}
}

// --- S3-specific Condition Key Tests ---

func TestS3ConditionKeys(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{
		S3Prefix: "images/",
		S3ACL:    "private",
	}

	if !pe.conditionMatches("StringEquals", "s3:prefix", "images/", ctx) {
		t.Error("s3:prefix condition should match")
	}
	if !pe.conditionMatches("StringEquals", "s3:x-amz-acl", "private", ctx) {
		t.Error("s3:x-amz-acl condition should match")
	}
}

// --- Edge Case Tests ---

func TestEmptyContext(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{}

	// Should not panic with empty context
	_ = pe.resolveConditionKey("aws:SourceIp", ctx)
	_ = pe.resolveConditionKey("aws:ResourceTag/Project", ctx)
	_ = pe.resolveVariableSubstitution("${aws:PrincipalTag/Team}", ctx)
}

func TestConditionsWithMissingKeys(t *testing.T) {
	pe := &PolicyEvaluator{}
	ctx := &EvalContext{}

	// StringEquals with missing key should return false
	if pe.conditionMatches("StringEquals", "aws:SourceIp", "10.0.0.1", ctx) {
		t.Error("StringEquals with missing key should return false")
	}

	// StringNotEquals with missing key should return true (empty != value)
	if !pe.conditionMatches("StringNotEquals", "aws:SourceIp", "10.0.0.1", ctx) {
		t.Error("StringNotEquals with missing key should return true")
	}
}
