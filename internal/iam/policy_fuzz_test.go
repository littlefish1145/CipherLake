package iam

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// FuzzConditionMatches fuzzes the condition matching logic with random operators, keys, and values.
func FuzzConditionMatches(f *testing.F) {
	operators := []string{
		"StringEquals", "StringNotEquals", "StringLike", "StringNotLike",
		"NumericEquals", "NumericNotEquals", "NumericLessThan", "NumericGreaterThan",
		"DateLessThan", "DateGreaterThan",
		"IpAddress", "NotIpAddress",
		"Bool", "ArnEquals", "ArnLike", "Null",
	}
	keys := []string{
		"aws:SourceIp", "aws:CurrentTime", "aws:UserAgent", "aws:PrincipalType",
		"s3:prefix", "s3:x-amz-acl",
		"aws:ResourceTag/Project", "aws:PrincipalTag/Team",
		"aws:RequestTag/Env",
	}
	values := []string{
		"192.168.1.1", "10.0.0.1", "172.16.0.1",
		"2024-01-01T00:00:00Z", "2025-06-15T12:30:00Z",
		"production", "staging", "development",
		"1", "100", "3.14", "-5",
		"true", "false",
		"arn:nexus:s3:::bucket/key",
		"arn:nexus:iam:::user/admin",
		"", "*",
	}

	// Seed corpus
	for _, op := range operators {
		for _, key := range keys {
			for _, val := range values {
				f.Add(op, key, val)
			}
		}
	}

	f.Fuzz(func(t *testing.T, operator, key, value string) {
		pe := &PolicyEvaluator{}
		ctx := &EvalContext{
			SourceIP:      "192.168.1.1",
			Time:          time.Now(),
			UserAgent:     "TestAgent/1.0",
			PrincipalType: "User",
			S3Prefix:      "images/",
			S3ACL:         "private",
			ResourceTags:  map[string]string{"Project": "Alpha"},
			PrincipalTags: map[string]string{"Team": "Backend"},
			RequestTags:   map[string]string{"Env": "production"},
			Conditions:    map[string]string{"custom:key": "custom-value"},
		}

		// This should never panic
		_ = pe.conditionMatches(operator, key, value, ctx)
	})
}

// FuzzPolicyEvaluation fuzzes full policy evaluation with random policy documents and contexts.
func FuzzPolicyEvaluation(f *testing.F) {
	// Seed corpus with various policy configurations
	seeds := []struct {
		effect   string
		action   string
		resource string
	}{
		{"Allow", "s3:GetObject", "arn:nexus:s3:::bucket/key"},
		{"Deny", "s3:DeleteObject", "arn:nexus:s3:::bucket/key"},
		{"Allow", "*", "*"},
		{"Allow", "s3:*", "arn:nexus:s3:::*"},
		{"Deny", "iam:*", "*"},
	}

	for _, seed := range seeds {
		f.Add(seed.effect, seed.action, seed.resource)
	}

	f.Fuzz(func(t *testing.T, effect, action, resource string) {
		// Limit input lengths to prevent excessive memory usage
		if len(effect) > 50 || len(action) > 200 || len(resource) > 500 {
			t.Skip()
		}

		// Ensure effect is valid
		if effect != "Allow" && effect != "Deny" {
			effect = "Allow"
		}

		stmt := Statement{
			Effect:   effect,
			Action:   StringOrSlice{action},
			Resource: StringOrSlice{resource},
		}

		ctx := &EvalContext{
			Principal: "test-user",
			Action:    "s3:GetObject",
			Resource:  "arn:nexus:s3:::mybucket/mykey",
			Time:      time.Now(),
		}

		pe := &PolicyEvaluator{}

		// This should never panic
		_ = pe.statementMatches(&stmt, ctx)
	})
}

// FuzzArnMatching fuzzes ARN pattern matching with random patterns and ARNs.
func FuzzArnMatching(f *testing.F) {
	patterns := []string{
		"arn:nexus:s3:::bucket/*",
		"arn:nexus:s3:::bucket/key",
		"arn:nexus:s3:::*",
		"*",
		"arn:nexus:s3:::bucket/prefix/*/suffix",
		"arn:nexus:iam:::user/*",
	}
	arns := []string{
		"arn:nexus:s3:::bucket/key",
		"arn:nexus:s3:::bucket/images/photo.png",
		"arn:nexus:iam:::user/admin",
		"arn:nexus:s3:::otherbucket/key",
	}

	for _, pattern := range patterns {
		for _, arn := range arns {
			f.Add(pattern, arn)
		}
	}

	f.Fuzz(func(t *testing.T, pattern, arn string) {
		// Limit input lengths
		if len(pattern) > 500 || len(arn) > 500 {
			t.Skip()
		}

		pe := &PolicyEvaluator{}

		// This should never panic or loop infinitely
		_ = pe.arnMatch(pattern, arn)
		_ = pe.matchResource(pattern, arn)
		_ = pe.globMatch(pattern, arn)
	})
}

// FuzzGlobMatch specifically targets the glob matching algorithm to ensure
// no infinite loops or panics with pathological inputs.
func FuzzGlobMatch(f *testing.F) {
	patterns := []string{"*", "a*", "*b", "a*b", "a?b", "a**b", "***"}
	strs := []string{"", "a", "ab", "abc", "axb", "aXXb"}

	for _, p := range patterns {
		for _, s := range strs {
			f.Add(p, s)
		}
	}

	f.Fuzz(func(t *testing.T, pattern, str string) {
		// Limit input lengths to prevent ReDoS-like issues
		if len(pattern) > 100 || len(str) > 100 {
			t.Skip()
		}

		// Reject patterns with excessive consecutive stars
		starCount := 0
		for _, c := range pattern {
			if c == '*' {
				starCount++
			}
		}
		if starCount > 20 {
			t.Skip()
		}

		pe := &PolicyEvaluator{}
		_ = pe.globMatch(pattern, str)
	})
}

// FuzzCompareValues fuzzes the compareValues function with all operators.
func FuzzCompareValues(f *testing.F) {
	operators := []string{
		"stringequals", "stringnotequals", "stringlike", "stringnotlike",
		"numericequals", "numericnotequals", "numericlessthan", "numericgreaterthan",
		"datelessthan", "dategreaterthan",
		"ipaddress", "notipaddress",
		"bool", "arnequals", "arnlike",
	}

	for _, op := range operators {
		f.Add(op, "test", "test")
		f.Add(op, "1", "2")
		f.Add(op, "192.168.1.1", "192.168.1.0/24")
	}

	f.Fuzz(func(t *testing.T, operator, actual, expected string) {
		// Limit input lengths
		if len(operator) > 50 || len(actual) > 200 || len(expected) > 200 {
			t.Skip()
		}

		pe := &PolicyEvaluator{}
		// This should never panic
		_ = pe.compareValues(strings.ToLower(operator), actual, expected)
	})
}

// FuzzConditionKeyResolution fuzzes the condition key resolution logic.
func FuzzConditionKeyResolution(f *testing.F) {
	keys := []string{
		"aws:sourceip", "aws:currenttime", "aws:useragent",
		"aws:principaltype", "s3:prefix", "s3:x-amz-acl",
		"aws:resourcetag/Project", "aws:principaltag/Team",
		"aws:requesttag/Env",
	}
	for _, key := range keys {
		f.Add(key)
	}

	f.Fuzz(func(t *testing.T, key string) {
		if len(key) > 200 {
			t.Skip()
		}

		pe := &PolicyEvaluator{}
		ctx := &EvalContext{
			SourceIP:      "10.0.0.1",
			Time:          time.Now(),
			UserAgent:     "FuzzAgent/0.1",
			PrincipalType: "User",
			S3Prefix:      "data/",
			S3ACL:         "public-read",
			ResourceTags:  map[string]string{"Project": "TestProject"},
			PrincipalTags: map[string]string{"Team": "TestTeam"},
			RequestTags:   map[string]string{"Env": "test"},
			Conditions:    map[string]string{key: "fuzz-value"},
		}

		_ = pe.resolveConditionKey(key, ctx)
	})
}

// FuzzVariableSubstitution fuzzes the variable substitution logic.
func FuzzVariableSubstitution(f *testing.F) {
	f.Add("${aws:PrincipalTag/Team}")
	f.Add("prefix-${aws:PrincipalTag/Team}-suffix")
	f.Add("no-substitution")
	f.Add("${aws:ResourceTag/Project}")
	f.Add("${invalid-ref}")

	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > 500 {
			t.Skip()
		}

		// Limit nested ${} patterns
		count := strings.Count(value, "${")
		if count > 10 {
			t.Skip()
		}

		pe := &PolicyEvaluator{}
		ctx := &EvalContext{
			PrincipalTags: map[string]string{"Team": "Backend"},
			ResourceTags:  map[string]string{"Project": "Alpha"},
		}

		_ = pe.resolveVariableSubstitution(value, ctx)
	})
}

// FuzzIpInRange fuzzes the CIDR matching logic.
func FuzzIpInRange(f *testing.F) {
	f.Add("192.168.1.1", "192.168.1.0/24")
	f.Add("10.0.0.1", "10.0.0.0/8")
	f.Add("::1", "::1/128")
	f.Add("2001:db8::1", "2001:db8::/32")

	f.Fuzz(func(t *testing.T, ip, cidr string) {
		if len(ip) > 50 || len(cidr) > 50 {
			t.Skip()
		}

		// This should never panic
		_ = ipInRange(ip, cidr)
	})
}

// FuzzNumericComparison fuzzes numeric comparison functions.
func FuzzNumericComparison(f *testing.F) {
	f.Add("1", "2")
	f.Add("3.14", "2.71")
	f.Add("-1", "0")
	f.Add("1e10", "1e9")
	f.Add("not-a-number", "also-not")

	f.Fuzz(func(t *testing.T, a, b string) {
		if len(a) > 50 || len(b) > 50 {
			t.Skip()
		}

		// These should never panic
		_ = numericEquals(a, b)
		_ = numericLessThan(a, b)
		_ = numericGreaterThan(a, b)
	})
}

// FuzzDateComparison fuzzes date comparison functions.
func FuzzDateComparison(f *testing.F) {
	f.Add("2024-01-01T00:00:00Z", "2025-01-01T00:00:00Z")
	f.Add("not-a-date", "also-not-a-date")

	f.Fuzz(func(t *testing.T, a, b string) {
		if len(a) > 100 || len(b) > 100 {
			t.Skip()
		}

		pe := &PolicyEvaluator{}
		_ = pe.dateLessThan(a, b)
		_ = pe.dateGreaterThan(a, b)
	})
}

// FuzzNullCondition fuzzes the Null condition operator.
func FuzzNullCondition(f *testing.F) {
	f.Add("aws:SourceIp", "true")
	f.Add("aws:UserAgent", "false")
	f.Add("aws:ResourceTag/Missing", "true")

	f.Fuzz(func(t *testing.T, key, expected string) {
		if len(key) > 200 || len(expected) > 50 {
			t.Skip()
		}

		pe := &PolicyEvaluator{}
		ctx := &EvalContext{
			SourceIP: "10.0.0.1",
			Time:     time.Now(),
		}

		_ = pe.nullConditionMatches(key, expected, ctx)
	})
}

// FuzzSimulate fuzzes the full Simulate path.
func FuzzSimulate(f *testing.F) {
	f.Add("s3:GetObject", "arn:nexus:s3:::bucket/key", "user1")
	f.Add("iam:CreateUser", "arn:nexus:iam:::user/test", "admin")

	f.Fuzz(func(t *testing.T, action, resource, principal string) {
		if len(action) > 200 || len(resource) > 500 || len(principal) > 200 {
			t.Skip()
		}

		ctx := &EvalContext{
			Principal: principal,
			Action:    action,
			Resource:  resource,
			Time:      time.Now(),
		}

		// Use a nil store evaluator (no policies loaded, will return implicit deny)
		pe := &PolicyEvaluator{store: nil}
		_ = pe.Simulate(ctx)
	})
}

// FuzzPolicyDocumentParsing fuzzes policy document parsing.
func FuzzPolicyDocumentParsing(f *testing.F) {
	f.Add(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`)
	f.Add(`{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"s3:*","Resource":"*","Condition":{"StringEquals":{"aws:SourceIp":"10.0.0.1"}}}]}`)

	f.Fuzz(func(t *testing.T, data string) {
		if len(data) > 5000 {
			t.Skip()
		}

		doc, err := ParsePolicyDocument([]byte(data))
		if err != nil {
			return // Invalid JSON is expected
		}

		// If parsing succeeded, try to evaluate it
		pe := &PolicyEvaluator{}
		ctx := &EvalContext{
			Principal: "test-user",
			Action:    "s3:GetObject",
			Resource:  "arn:nexus:s3:::bucket/key",
			Time:      time.Now(),
		}

		for i := range doc.Statement {
			_ = pe.statementMatches(&doc.Statement[i], ctx)
		}
	})
}

// FuzzABAC fuzzes ABAC tag matching.
func FuzzABAC(f *testing.F) {
	f.Add("Project", "Project")
	f.Add("Environment", "Team")
	f.Add("", "")

	f.Fuzz(func(t *testing.T, resourceTagKey, principalTagKey string) {
		if len(resourceTagKey) > 100 || len(principalTagKey) > 100 {
			t.Skip()
		}

		ctx := &EvalContext{
			ResourceTags:  map[string]string{"Project": "Alpha", "Environment": "production"},
			PrincipalTags: map[string]string{"Project": "Alpha", "Team": "Backend"},
		}

		_ = ABACMatch(ctx, resourceTagKey, principalTagKey)
		_ = ABACMatchLiteral(ctx, resourceTagKey, "production")
		_ = ResolveResourceTag(ctx, resourceTagKey)
		_ = ResolvePrincipalTag(ctx, principalTagKey)
	})
}

// FuzzSCP fuzzes SCP evaluation.
func FuzzSCP(f *testing.F) {
	f.Add("Allow", "s3:*", "*")
	f.Add("Deny", "iam:*", "*")

	f.Fuzz(func(t *testing.T, effect, action, resource string) {
		if len(effect) > 20 || len(action) > 200 || len(resource) > 500 {
			t.Skip()
		}

		if effect != "Allow" && effect != "Deny" {
			effect = "Allow"
		}

		scp := &PolicyDocument{
			Version: PolicyVersion,
			Statement: []Statement{
				{
					Effect:   effect,
					Action:   StringOrSlice{action},
					Resource: StringOrSlice{resource},
				},
			},
		}

		store := &IAMStore{scp: scp}
		pe := &PolicyEvaluator{store: store}

		ctx := &EvalContext{
			Principal: "test-user",
			Action:    "s3:GetObject",
			Resource:  "arn:nexus:s3:::bucket/key",
			Time:      time.Now(),
		}

		_ = pe.evaluateSCP(ctx)
	})
}

// FuzzBoundary fuzzes permission boundary evaluation.
func FuzzBoundary(f *testing.F) {
	f.Add("Allow", "s3:*", "*")
	f.Add("Deny", "s3:DeleteObject", "*")

	f.Fuzz(func(t *testing.T, effect, action, resource string) {
		if len(effect) > 20 || len(action) > 200 || len(resource) > 500 {
			t.Skip()
		}

		if effect != "Allow" && effect != "Deny" {
			effect = "Allow"
		}

		boundary := PolicyDocument{
			Version: PolicyVersion,
			Statement: []Statement{
				{
					Effect:   effect,
					Action:   StringOrSlice{action},
					Resource: StringOrSlice{resource},
				},
			},
		}

		pe := &PolicyEvaluator{}
		ctx := &EvalContext{
			Principal: "test-user",
			Action:    "s3:GetObject",
			Resource:  "arn:nexus:s3:::bucket/key",
			Time:      time.Now(),
		}

		_ = pe.evaluateBoundary(ctx, boundary)
	})
}

// FuzzStringOrSlice fuzzes JSON unmarshaling of StringOrSlice.
func FuzzStringOrSlice(f *testing.F) {
	f.Add(`"single-value"`)
	f.Add(`["value1","value2"]`)
	f.Add(`null`)
	f.Add(`123`)
	f.Add(`{}`)

	f.Fuzz(func(t *testing.T, data string) {
		if len(data) > 1000 {
			t.Skip()
		}

		var s StringOrSlice
		_ = s.UnmarshalJSON([]byte(data))

		// If unmarshal succeeded, try marshaling back
		if len(s) > 0 {
			_, _ = s.MarshalJSON()
		}
	})
}

// FuzzExtractBucket fuzzes the bucket extraction from ARN.
func FuzzExtractBucket(f *testing.F) {
	f.Add("arn:nexus:s3:::mybucket/mykey")
	f.Add("arn:nexus:s3:::mybucket")
	f.Add("arn:nexus:iam:::user/admin")
	f.Add("not-an-arn")

	f.Fuzz(func(t *testing.T, resource string) {
		if len(resource) > 500 {
			t.Skip()
		}

		_ = extractBucketFromResource(resource)
		bucket, key := ResourceToS3Path(resource)
		_ = bucket
		_ = key
	})
}

// FuzzMakeARN fuzzes ARN construction.
func FuzzMakeARN(f *testing.F) {
	f.Add("s3", "bucket/key")
	f.Add("iam", "user/admin")

	f.Fuzz(func(t *testing.T, service, resource string) {
		if len(service) > 50 || len(resource) > 200 {
			t.Skip()
		}

		arn := MakeARN(service, resource)
		_ = fmt.Sprintf("ARN: %s", arn)
	})
}
