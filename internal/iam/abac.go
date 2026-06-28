package iam

// ABAC (Attribute-Based Access Control) support for IAM policy evaluation.
//
// ABAC enables authorization decisions based on attributes (tags) attached to
// principals, resources, and requests. This file provides the tag resolution
// and matching logic used by the condition evaluator in policy.go.

// ResolveResourceTag looks up a tag key on the resource in the evaluation context.
func ResolveResourceTag(ctx *EvalContext, tagKey string) string {
	if ctx == nil || ctx.ResourceTags == nil {
		return ""
	}
	return ctx.ResourceTags[tagKey]
}

// ResolvePrincipalTag looks up a tag key on the principal in the evaluation context.
func ResolvePrincipalTag(ctx *EvalContext, tagKey string) string {
	if ctx == nil || ctx.PrincipalTags == nil {
		return ""
	}
	return ctx.PrincipalTags[tagKey]
}

// ResolveRequestTag looks up a tag key from the request in the evaluation context.
func ResolveRequestTag(ctx *EvalContext, tagKey string) string {
	if ctx == nil || ctx.RequestTags == nil {
		return ""
	}
	return ctx.RequestTags[tagKey]
}

// ABACMatch checks if a resource tag matches a principal tag (tag-based authorization).
// This is used for policies like:
//
//	"Condition": {
//	  "StringEquals": {
//	    "aws:ResourceTag/Project": "${aws:PrincipalTag/Project}"
//	  }
//	}
//
// The variable substitution ${aws:PrincipalTag/X} is resolved by
// resolveVariableSubstitution in policy.go, and the tag key resolution
// aws:ResourceTag/X is handled by resolveConditionKey.
//
// This function provides a convenience wrapper for the common ABAC pattern
// of matching a resource tag against a principal tag.
func ABACMatch(ctx *EvalContext, resourceTagKey, principalTagKey string) bool {
	resourceTag := ResolveResourceTag(ctx, resourceTagKey)
	principalTag := ResolvePrincipalTag(ctx, principalTagKey)
	if resourceTag == "" || principalTag == "" {
		return false
	}
	return resourceTag == principalTag
}

// ABACMatchLiteral checks if a resource tag matches a literal value.
// This is used for policies like:
//
//	"Condition": {
//	  "StringEquals": {
//	    "aws:ResourceTag/Environment": "production"
//	  }
//	}
func ABACMatchLiteral(ctx *EvalContext, resourceTagKey, expectedValue string) bool {
	resourceTag := ResolveResourceTag(ctx, resourceTagKey)
	if resourceTag == "" {
		return false
	}
	return resourceTag == expectedValue
}
