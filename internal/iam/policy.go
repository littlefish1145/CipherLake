package iam

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PolicyEvaluator evaluates IAM policies against a request context
type PolicyEvaluator struct {
	store *IAMStore
}

// NewPolicyEvaluator creates a new policy evaluator
func NewPolicyEvaluator(store *IAMStore) *PolicyEvaluator {
	return &PolicyEvaluator{store: store}
}

// Evaluate evaluates all applicable policies for a request
// Evaluation order (AWS spec + SCP + Boundary):
// 0. Check SCP for explicit Deny → Deny (overrides everything)
// 1. Check for explicit Deny in all applicable policies -> Deny
// 2. Check for explicit Allow in identity-based policies -> candidate Allow
// 3. Check permission boundary -> if boundary doesn't allow, downgrade to Deny
// 4. Check for explicit Allow in resource-based policies -> Allow
// 5. Default ImplicitDeny
func (pe *PolicyEvaluator) Evaluate(ctx *EvalContext) *EvalResult {
	// Phase 0: Check SCP first (organization-level gate)
	if pe.store != nil && pe.store.scp != nil {
		scpResult := pe.evaluateSCP(ctx)
		if scpResult.Decision == DecisionDeny {
			scpResult.PolicyType = "scp"
			return scpResult
		}
		if scpResult.Decision != DecisionAllow {
			// SCP doesn't allow, deny
			return &EvalResult{
				Decision:   DecisionDeny,
				PolicyType: "scp",
				Details:    "Denied by Service Control Policy (no Allow statement matches)",
			}
		}
	}

	// If no store is available, we can't evaluate policies
	if pe.store == nil {
		return &EvalResult{
			Decision:   DecisionImplicitDeny,
			PolicyType: "",
			Details:    "No policy store available (implicit deny)",
		}
	}

	// Collect all applicable policy documents
	var identityPolicies []PolicyDocument
	var resourcePolicies []PolicyDocument
	var identityPolicyNames []string

	// 1. Get user's inline policies and attached policies
	user, err := pe.store.GetUser(ctx.Principal)
	if err == nil && user != nil {
		// Inline policies
		for _, ip := range user.InlinePolicies {
			identityPolicies = append(identityPolicies, ip)
			identityPolicyNames = append(identityPolicyNames, ip.Id)
		}

		// Attached managed policies
		for _, policyName := range user.AttachedPolicies {
			policy, err := pe.store.GetPolicy(policyName)
			if err == nil {
				identityPolicies = append(identityPolicies, policy.Document)
				identityPolicyNames = append(identityPolicyNames, policyName)
			}
		}

		// Group policies
		for _, groupName := range user.Groups {
			group, err := pe.store.GetGroup(groupName)
			if err == nil {
				for _, policyName := range group.AttachedPolicies {
					policy, err := pe.store.GetPolicy(policyName)
					if err == nil {
						identityPolicies = append(identityPolicies, policy.Document)
						identityPolicyNames = append(identityPolicyNames, policyName)
					}
				}
			}
		}
	}

	// 2. Get bucket policy (resource-based)
	bucketName := extractBucketFromResource(ctx.Resource)
	if bucketName != "" {
		bp, err := pe.store.GetBucketPolicy(bucketName)
		if err == nil {
			resourcePolicies = append(resourcePolicies, bp.Document)
		}
	}

	// Phase 1: Check for explicit Deny across ALL policies
	allPolicies := append(identityPolicies, resourcePolicies...)
	for _, doc := range allPolicies {
		for _, stmt := range doc.Statement {
			if stmt.Effect == EffectDeny {
				if pe.statementMatches(&stmt, ctx) {
					return &EvalResult{
						Decision:   DecisionDeny,
						MatchedBy:  stmt.Sid,
						PolicyType: "identity",
						Details:    fmt.Sprintf("Explicitly denied by statement '%s'", stmt.Sid),
					}
				}
			}
		}
	}

	// Phase 2: Check for explicit Allow in identity-based policies
	var identityAllow *EvalResult
	for i, doc := range identityPolicies {
		for _, stmt := range doc.Statement {
			if stmt.Effect == EffectAllow {
				if pe.statementMatches(&stmt, ctx) {
					policyName := ""
					if i < len(identityPolicyNames) {
						policyName = identityPolicyNames[i]
					}
					identityAllow = &EvalResult{
						Decision:   DecisionAllow,
						MatchedBy:  stmt.Sid,
						PolicyType: "identity",
						PolicyName: policyName,
						Details:    fmt.Sprintf("Allowed by identity policy '%s' statement '%s'", policyName, stmt.Sid),
					}
					break
				}
			}
		}
		if identityAllow != nil {
			break
		}
	}

	if identityAllow != nil {
		// Phase 3: Check permission boundary
		if user != nil && user.PermissionBoundary != "" {
			boundaryPolicy, err := pe.store.GetPolicy(user.PermissionBoundary)
			if err == nil {
				boundaryResult := pe.evaluateBoundary(ctx, boundaryPolicy.Document)
				if boundaryResult.Decision != DecisionAllow {
					return &EvalResult{
						Decision:   DecisionDeny,
						MatchedBy:  boundaryResult.MatchedBy,
						PolicyType: "boundary",
						PolicyName: user.PermissionBoundary,
						Details:    fmt.Sprintf("Denied by permission boundary '%s' (identity policy allows but boundary does not)", user.PermissionBoundary),
					}
				}
			}
		}
		return identityAllow
	}

	// Phase 4: Check for explicit Allow in resource-based policies
	for _, doc := range resourcePolicies {
		for _, stmt := range doc.Statement {
			if stmt.Effect == EffectAllow {
				if pe.statementMatches(&stmt, ctx) {
					return &EvalResult{
						Decision:   DecisionAllow,
						MatchedBy:  stmt.Sid,
						PolicyType: "resource",
						Details:    fmt.Sprintf("Allowed by resource policy statement '%s'", stmt.Sid),
					}
				}
			}
		}
	}

	// Phase 5: Default implicit deny
	return &EvalResult{
		Decision:   DecisionImplicitDeny,
		PolicyType: "",
		Details:    "No matching Allow statement found (implicit deny)",
	}
}

// EvaluateWithBoundary evaluates policies with explicit permission boundary checking
func (pe *PolicyEvaluator) EvaluateWithBoundary(ctx *EvalContext, user *IAMUser) *EvalResult {
	// Step 1: Check explicit Deny in all policies → Deny
	var identityPolicies []PolicyDocument
	var identityPolicyNames []string

	if user != nil {
		for _, ip := range user.InlinePolicies {
			identityPolicies = append(identityPolicies, ip)
			identityPolicyNames = append(identityPolicyNames, ip.Id)
		}
		for _, policyName := range user.AttachedPolicies {
			policy, err := pe.store.GetPolicy(policyName)
			if err == nil {
				identityPolicies = append(identityPolicies, policy.Document)
				identityPolicyNames = append(identityPolicyNames, policyName)
			}
		}
		for _, groupName := range user.Groups {
			group, err := pe.store.GetGroup(groupName)
			if err == nil {
				for _, policyName := range group.AttachedPolicies {
					policy, err := pe.store.GetPolicy(policyName)
					if err == nil {
						identityPolicies = append(identityPolicies, policy.Document)
						identityPolicyNames = append(identityPolicyNames, policyName)
					}
				}
			}
		}
	}

	// Resource-based policies
	var resourcePolicies []PolicyDocument
	bucketName := extractBucketFromResource(ctx.Resource)
	if bucketName != "" {
		bp, err := pe.store.GetBucketPolicy(bucketName)
		if err == nil {
			resourcePolicies = append(resourcePolicies, bp.Document)
		}
	}

	// Step 1: Check explicit Deny
	allPolicies := append(identityPolicies, resourcePolicies...)
	for _, doc := range allPolicies {
		for _, stmt := range doc.Statement {
			if stmt.Effect == EffectDeny && pe.statementMatches(&stmt, ctx) {
				return &EvalResult{
					Decision:   DecisionDeny,
					MatchedBy:  stmt.Sid,
					PolicyType: "identity",
					Details:    fmt.Sprintf("Explicitly denied by statement '%s'", stmt.Sid),
				}
			}
		}
	}

	// Step 2: Check Allow in identity policies → candidate Allow
	var identityAllow *EvalResult
	for i, doc := range identityPolicies {
		for _, stmt := range doc.Statement {
			if stmt.Effect == EffectAllow && pe.statementMatches(&stmt, ctx) {
				policyName := ""
				if i < len(identityPolicyNames) {
					policyName = identityPolicyNames[i]
				}
				identityAllow = &EvalResult{
					Decision:   DecisionAllow,
					MatchedBy:  stmt.Sid,
					PolicyType: "identity",
					PolicyName: policyName,
					Details:    fmt.Sprintf("Allowed by identity policy '%s' statement '%s'", policyName, stmt.Sid),
				}
				break
			}
		}
		if identityAllow != nil {
			break
		}
	}

	// Step 3: Check permission boundary → if boundary doesn't allow, downgrade to Deny
	if identityAllow != nil && user != nil && user.PermissionBoundary != "" {
		boundaryPolicy, err := pe.store.GetPolicy(user.PermissionBoundary)
		if err == nil {
			boundaryResult := pe.evaluateBoundary(ctx, boundaryPolicy.Document)
			if boundaryResult.Decision != DecisionAllow {
				return &EvalResult{
					Decision:   DecisionDeny,
					MatchedBy:  boundaryResult.MatchedBy,
					PolicyType: "boundary",
					PolicyName: user.PermissionBoundary,
					Details:    fmt.Sprintf("Denied by permission boundary '%s' (identity policy allows but boundary does not)", user.PermissionBoundary),
				}
			}
		}
	}

	if identityAllow != nil {
		return identityAllow
	}

	// Step 4: Check Allow in resource policies
	for _, doc := range resourcePolicies {
		for _, stmt := range doc.Statement {
			if stmt.Effect == EffectAllow && pe.statementMatches(&stmt, ctx) {
				return &EvalResult{
					Decision:   DecisionAllow,
					MatchedBy:  stmt.Sid,
					PolicyType: "resource",
					Details:    fmt.Sprintf("Allowed by resource policy statement '%s'", stmt.Sid),
				}
			}
		}
	}

	// Step 5: Default implicit deny
	return &EvalResult{
		Decision:   DecisionImplicitDeny,
		PolicyType: "",
		Details:    "No matching Allow statement found (implicit deny)",
	}
}

// evaluateBoundary checks if a permission boundary allows the request
func (pe *PolicyEvaluator) evaluateBoundary(ctx *EvalContext, boundaryDoc PolicyDocument) *EvalResult {
	// Check for explicit Deny in boundary
	for _, stmt := range boundaryDoc.Statement {
		if stmt.Effect == EffectDeny && pe.statementMatches(&stmt, ctx) {
			return &EvalResult{
				Decision:  DecisionDeny,
				MatchedBy: stmt.Sid,
			}
		}
	}
	// Check for Allow in boundary
	for _, stmt := range boundaryDoc.Statement {
		if stmt.Effect == EffectAllow && pe.statementMatches(&stmt, ctx) {
			return &EvalResult{
				Decision:  DecisionAllow,
				MatchedBy: stmt.Sid,
			}
		}
	}
	// Boundary doesn't explicitly allow → implicit deny
	return &EvalResult{
		Decision: DecisionImplicitDeny,
	}
}

// evaluateSCP checks the Service Control Policy
func (pe *PolicyEvaluator) evaluateSCP(ctx *EvalContext) *EvalResult {
	if pe.store == nil || pe.store.scp == nil {
		return &EvalResult{Decision: DecisionAllow}
	}

	// Check for explicit Deny in SCP
	for _, stmt := range pe.store.scp.Statement {
		if stmt.Effect == EffectDeny && pe.statementMatches(&stmt, ctx) {
			return &EvalResult{
				Decision:  DecisionDeny,
				MatchedBy: stmt.Sid,
				Details:   fmt.Sprintf("Denied by SCP statement '%s'", stmt.Sid),
			}
		}
	}

	// Check for Allow in SCP
	for _, stmt := range pe.store.scp.Statement {
		if stmt.Effect == EffectAllow && pe.statementMatches(&stmt, ctx) {
			return &EvalResult{
				Decision:  DecisionAllow,
				MatchedBy: stmt.Sid,
			}
		}
	}

	// SCP doesn't allow → implicit deny
	return &EvalResult{
		Decision: DecisionImplicitDeny,
		Details:  "No Allow statement in SCP matches this request",
	}
}

// EvaluateWithTempCreds evaluates policies for temporary credentials (STS)
func (pe *PolicyEvaluator) EvaluateWithTempCreds(ctx *EvalContext, rolePolicies []string, inlinePolicy *PolicyDocument) *EvalResult {
	var identityPolicies []PolicyDocument

	// Role's permission policies
	for _, policyName := range rolePolicies {
		policy, err := pe.store.GetPolicy(policyName)
		if err == nil {
			identityPolicies = append(identityPolicies, policy.Document)
		}
	}

	// Inline policy from AssumeRole (scoped down)
	if inlinePolicy != nil {
		identityPolicies = append(identityPolicies, *inlinePolicy)
	}

	// Resource-based policies
	var resourcePolicies []PolicyDocument
	bucketName := extractBucketFromResource(ctx.Resource)
	if bucketName != "" {
		bp, err := pe.store.GetBucketPolicy(bucketName)
		if err == nil {
			resourcePolicies = append(resourcePolicies, bp.Document)
		}
	}

	// Same evaluation logic
	allPolicies := append(identityPolicies, resourcePolicies...)
	for _, doc := range allPolicies {
		for _, stmt := range doc.Statement {
			if stmt.Effect == EffectDeny && pe.statementMatches(&stmt, ctx) {
				return &EvalResult{Decision: DecisionDeny, MatchedBy: stmt.Sid, PolicyType: "identity"}
			}
		}
	}

	for _, doc := range identityPolicies {
		for _, stmt := range doc.Statement {
			if stmt.Effect == EffectAllow && pe.statementMatches(&stmt, ctx) {
				return &EvalResult{Decision: DecisionAllow, MatchedBy: stmt.Sid, PolicyType: "identity"}
			}
		}
	}

	for _, doc := range resourcePolicies {
		for _, stmt := range doc.Statement {
			if stmt.Effect == EffectAllow && pe.statementMatches(&stmt, ctx) {
				return &EvalResult{Decision: DecisionAllow, MatchedBy: stmt.Sid, PolicyType: "resource"}
			}
		}
	}

	return &EvalResult{Decision: DecisionImplicitDeny}
}

// Simulate evaluates a request and returns a detailed SimulateResponse
func (pe *PolicyEvaluator) Simulate(ctx *EvalContext) *SimulateResponse {
	result := pe.Evaluate(ctx)
	return &SimulateResponse{
		Decision:   result.Decision.String(),
		MatchedBy:  result.MatchedBy,
		PolicyType: result.PolicyType,
		Details:    result.Details,
	}
}

// statementMatches checks if a statement matches the evaluation context
func (pe *PolicyEvaluator) statementMatches(stmt *Statement, ctx *EvalContext) bool {
	// Check Action
	if !pe.actionMatches(stmt.Action, ctx.Action) {
		// Check NotAction (inverse match)
		if len(stmt.NotAction) > 0 {
			if pe.actionMatches(stmt.NotAction, ctx.Action) {
				return false // Action is in NotAction list, so this statement doesn't apply
			}
			// Action is NOT in NotAction, so statement applies (fall through to Resource check)
		} else {
			return false
		}
	}

	// Check Resource
	if !pe.resourceMatches(stmt.Resource, ctx.Resource) {
		// Check NotResource
		if len(stmt.NotResource) > 0 {
			if pe.resourceMatches(stmt.NotResource, ctx.Resource) {
				return false
			}
		} else {
			return false
		}
	}

	// Check Principal (for bucket policies)
	if stmt.Principal != nil {
		if !pe.principalMatches(stmt.Principal, ctx.Principal) {
			return false
		}
	}

	// Check Conditions
	if len(stmt.Condition) > 0 {
		if !pe.conditionsMatch(stmt.Condition, ctx) {
			return false
		}
	}

	return true
}

// actionMatches checks if the requested action matches any of the policy actions
func (pe *PolicyEvaluator) actionMatches(policyActions []string, requestAction string) bool {
	for _, action := range policyActions {
		if pe.matchAction(action, requestAction) {
			return true
		}
	}
	return false
}

// matchAction matches a single action pattern against a request action
func (pe *PolicyEvaluator) matchAction(pattern, action string) bool {
	if pattern == "*" {
		return true
	}

	pattern = strings.ToLower(pattern)
	action = strings.ToLower(action)

	if pattern == action {
		return true
	}

	// Handle service:* (e.g., s3:*)
	if strings.HasSuffix(pattern, ":*") {
		service := strings.TrimSuffix(pattern, ":*")
		return strings.HasPrefix(action, service+":")
	}

	// Handle wildcard patterns like s3:Get* or s3:List*
	if strings.Contains(pattern, "*") || strings.Contains(pattern, "?") {
		return pe.globMatch(pattern, action)
	}

	return false
}

// resourceMatches checks if the requested resource matches any of the policy resources
func (pe *PolicyEvaluator) resourceMatches(policyResources []string, requestResource string) bool {
	for _, resource := range policyResources {
		if pe.matchResource(resource, requestResource) {
			return true
		}
	}
	return false
}

// matchResource matches a resource ARN pattern against a request resource ARN
func (pe *PolicyEvaluator) matchResource(pattern, resource string) bool {
	if pattern == "*" {
		return true
	}

	// Exact match
	if pattern == resource {
		return true
	}

	// Handle wildcards in the resource path
	if strings.Contains(pattern, "*") || strings.Contains(pattern, "?") {
		return pe.globMatch(pattern, resource)
	}

	return false
}

// globMatch performs simple glob matching with * and ?
func (pe *PolicyEvaluator) globMatch(pattern, str string) bool {
	pi, si := 0, 0
	starIdx := -1
	matchIdx := 0

	for si < len(str) {
		if pi < len(pattern) && (pattern[pi] == str[si] || pattern[pi] == '?') {
			pi++
			si++
		} else if pi < len(pattern) && pattern[pi] == '*' {
			starIdx = pi
			matchIdx = si
			pi++
		} else if starIdx != -1 {
			pi = starIdx + 1
			matchIdx++
			si = matchIdx
		} else {
			return false
		}
	}

	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}

	return pi == len(pattern)
}

// principalMatches checks if the request principal matches the policy principal
func (pe *PolicyEvaluator) principalMatches(principal *Principal, requestPrincipal string) bool {
	// Check AWS principals
	for _, p := range principal.AWS {
		if p == "*" {
			return true
		}
		if p == requestPrincipal {
			return true
		}
		// Match user ARN
		if strings.HasPrefix(requestPrincipal, ARNPrefix) {
			if pe.globMatch(p, requestPrincipal) {
				return true
			}
		}
	}
	return len(principal.AWS) == 0 && len(principal.Service) == 0 && len(principal.Federated) == 0
}

// conditionsMatch evaluates condition blocks
func (pe *PolicyEvaluator) conditionsMatch(conditions map[string]map[string]interface{}, ctx *EvalContext) bool {
	for operator, conditionsByKey := range conditions {
		for key, expectedValue := range conditionsByKey {
			if !pe.conditionMatches(operator, key, expectedValue, ctx) {
				return false
			}
		}
	}
	return true
}

// conditionMatches evaluates a single condition
func (pe *PolicyEvaluator) conditionMatches(operator, key string, expectedValue interface{}, ctx *EvalContext) bool {
	opLower := strings.ToLower(operator)

	// Handle Null operator specially - it checks key absence/presence
	if opLower == "null" {
		return pe.nullConditionMatches(key, expectedValue, ctx)
	}

	// Get actual value from context using key resolution
	actualValue := pe.resolveConditionKey(key, ctx)

	// Normalize expected value
	var expectedStr string
	switch v := expectedValue.(type) {
	case string:
		expectedStr = pe.resolveVariableSubstitution(v, ctx)
	case []interface{}:
		// For multi-value conditions, any match suffices
		for _, item := range v {
			if s, ok := item.(string); ok {
				resolved := pe.resolveVariableSubstitution(s, ctx)
				if pe.compareValues(opLower, actualValue, resolved) {
					return true
				}
			}
		}
		return false
	default:
		expectedStr = fmt.Sprintf("%v", v)
	}

	return pe.compareValues(opLower, actualValue, expectedStr)
}

// resolveConditionKey resolves a condition key to its actual value from the context
func (pe *PolicyEvaluator) resolveConditionKey(key string, ctx *EvalContext) string {
	keyLower := strings.ToLower(key)

	// Handle tag-based keys
	if strings.HasPrefix(keyLower, "aws:resourcetag/") {
		tagKey := key[len("aws:resourcetag/"):]
		if ctx.ResourceTags != nil {
			return ctx.ResourceTags[tagKey]
		}
		return ""
	}
	if strings.HasPrefix(keyLower, "aws:principaltag/") {
		tagKey := key[len("aws:principaltag/"):]
		if ctx.PrincipalTags != nil {
			return ctx.PrincipalTags[tagKey]
		}
		return ""
	}
	if strings.HasPrefix(keyLower, "aws:requesttag/") {
		tagKey := key[len("aws:requesttag/"):]
		if ctx.RequestTags != nil {
			return ctx.RequestTags[tagKey]
		}
		return ""
	}

	switch keyLower {
	case "aws:sourceip":
		return ctx.SourceIP
	case "aws:currenttime":
		if ctx.Time.IsZero() {
			return ""
		}
		return ctx.Time.Format(time.RFC3339)
	case "aws:useragent":
		return ctx.UserAgent
	case "aws:principaltype":
		return ctx.PrincipalType
	case "s3:prefix":
		return ctx.S3Prefix
	case "s3:x-amz-acl":
		return ctx.S3ACL
	default:
		if ctx.Conditions != nil {
			return ctx.Conditions[key]
		}
	}
	return ""
}

// resolveVariableSubstitution resolves ${aws:PrincipalTag/X} style variable references
func (pe *PolicyEvaluator) resolveVariableSubstitution(value string, ctx *EvalContext) string {
	if !strings.Contains(value, "${") {
		return value
	}

	// Find and replace all ${...} patterns
	result := value
	for {
		start := strings.Index(result, "${")
		if start == -1 {
			break
		}
		end := strings.Index(result[start:], "}")
		if end == -1 {
			break
		}
		end += start

		varRef := result[start+2 : end]
		resolved := pe.resolveConditionKey(varRef, ctx)

		result = result[:start] + resolved + result[end+1:]
	}
	return result
}

// nullConditionMatches handles the Null condition operator
// Null checks whether a condition key is absent (true) or present (false)
func (pe *PolicyEvaluator) nullConditionMatches(key string, expectedValue interface{}, ctx *EvalContext) bool {
	actualValue := pe.resolveConditionKey(key, ctx)
	keyIsAbsent := actualValue == ""

	var expectedNull bool
	switch v := expectedValue.(type) {
	case string:
		expectedNull = strings.ToLower(v) == "true"
	case bool:
		expectedNull = v
	default:
		expectedNull = false
	}

	return keyIsAbsent == expectedNull
}

// compareValues compares actual vs expected using the condition operator
func (pe *PolicyEvaluator) compareValues(operator, actual, expected string) bool {
	switch operator {
	case "stringequals":
		return actual == expected
	case "stringnotequals":
		return actual != expected
	case "stringlike":
		return pe.globMatch(expected, actual)
	case "stringnotlike":
		return !pe.globMatch(expected, actual)
	case "numericequals":
		return numericEquals(actual, expected)
	case "numericnotequals":
		return !numericEquals(actual, expected)
	case "numericlessthan":
		return numericLessThan(actual, expected)
	case "numericgreaterthan":
		return numericGreaterThan(actual, expected)
	case "datelessthan":
		return pe.dateLessThan(actual, expected)
	case "dategreaterthan":
		return pe.dateGreaterThan(actual, expected)
	case "ipaddress":
		return ipInRange(actual, expected)
	case "notipaddress":
		return !ipInRange(actual, expected)
	case "bool":
		return strings.ToLower(actual) == strings.ToLower(expected)
	case "arnequals", "arnlike":
		return pe.arnMatch(expected, actual)
	default:
		return actual == expected
	}
}

// ipInRange checks if an IP is in a CIDR range using net/netip
func ipInRange(ip, cidr string) bool {
	if cidr == "*" {
		return true
	}
	if !strings.Contains(cidr, "/") {
		// Not a CIDR, treat as exact IP match
		return ip == cidr
	}

	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return false
	}
	return prefix.Contains(addr)
}

// dateLessThan checks if a date string is less than another
func (pe *PolicyEvaluator) dateLessThan(actual, expected string) bool {
	a, err1 := time.Parse(time.RFC3339, actual)
	b, err2 := time.Parse(time.RFC3339, expected)
	if err1 != nil || err2 != nil {
		return actual < expected
	}
	return a.Before(b)
}

// dateGreaterThan checks if a date string is greater than another
func (pe *PolicyEvaluator) dateGreaterThan(actual, expected string) bool {
	a, err1 := time.Parse(time.RFC3339, actual)
	b, err2 := time.Parse(time.RFC3339, expected)
	if err1 != nil || err2 != nil {
		return actual > expected
	}
	return a.After(b)
}

// numericEquals checks numeric equality
func numericEquals(actual, expected string) bool {
	a, err1 := strconv.ParseFloat(actual, 64)
	b, err2 := strconv.ParseFloat(expected, 64)
	if err1 != nil || err2 != nil {
		return actual == expected
	}
	return a == b
}

// numericLessThan checks if actual < expected
func numericLessThan(actual, expected string) bool {
	a, err1 := strconv.ParseFloat(actual, 64)
	b, err2 := strconv.ParseFloat(expected, 64)
	if err1 != nil || err2 != nil {
		return actual < expected
	}
	return a < b
}

// numericGreaterThan checks if actual > expected
func numericGreaterThan(actual, expected string) bool {
	a, err1 := strconv.ParseFloat(actual, 64)
	b, err2 := strconv.ParseFloat(expected, 64)
	if err1 != nil || err2 != nil {
		return actual > expected
	}
	return a > b
}

// arnMatch matches an ARN pattern against an actual ARN
// ARN matching supports wildcards (* and ?) in the resource portion
func (pe *PolicyEvaluator) arnMatch(pattern, arn string) bool {
	if pattern == "*" {
		return true
	}
	if pattern == arn {
		return true
	}
	// Use glob matching for wildcard patterns
	if strings.Contains(pattern, "*") || strings.Contains(pattern, "?") {
		return pe.globMatch(pattern, arn)
	}
	// Case-insensitive comparison for ARN parts
	return strings.EqualFold(pattern, arn)
}

// extractBucketFromResource extracts bucket name from an ARN resource
func extractBucketFromResource(resource string) string {
	// arn:nexus:s3:::bucket-name/key -> bucket-name
	// arn:nexus:s3:::bucket-name -> bucket-name
	if !strings.HasPrefix(resource, ARNPrefix+ServiceS3+":::") {
		return ""
	}
	rest := strings.TrimPrefix(resource, ARNPrefix+ServiceS3+":::")
	parts := strings.SplitN(rest, "/", 2)
	return parts[0]
}

// ParsePolicyDocument parses a JSON policy document
func ParsePolicyDocument(data []byte) (*PolicyDocument, error) {
	var doc PolicyDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse policy document: %w", err)
	}
	return &doc, nil
}

// ParsePolicyDocumentFromString parses a JSON policy document from string
func ParsePolicyDocumentFromString(data string) (*PolicyDocument, error) {
	return ParsePolicyDocument([]byte(data))
}

// ResourceToS3Path converts an ARN resource to bucket/key
func ResourceToS3Path(resource string) (bucket, key string) {
	if !strings.HasPrefix(resource, ARNPrefix+ServiceS3+":::") {
		return "", ""
	}
	rest := strings.TrimPrefix(resource, ARNPrefix+ServiceS3+":::")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

// S3PathToResource converts bucket/key to an ARN resource
func S3PathToResource(bucket, key string) string {
	if key == "" {
		return MakeBucketARN(bucket)
	}
	return MakeObjectARN(bucket, filepath.ToSlash(key))
}

// IsAdminAction checks if an action is an IAM/admin action
func IsAdminAction(action string) bool {
	return strings.HasPrefix(action, "iam:") || strings.HasPrefix(action, "sts:") || strings.HasPrefix(action, "admin:")
}
