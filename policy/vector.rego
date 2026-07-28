package cipherlake.vector

# Default deny for all vector operations
default allow = false

# ============================================================
# Vector Search Access Control Policy
# ============================================================
# Actions:
#   vector:Search  - Search the vector index
#   vector:Index   - Index documents (auto on upload, or manual)
#   vector:Delete  - Remove documents from the index
#   vector:Manage  - Rebuild index, view stats, change config
#   vector:*       - All vector operations
#
# Resources:
#   arn:cipherlake:vector:::index/<bucket>           - Bucket-level index
#   arn:cipherlake:vector:::index/<bucket>/<key>     - Specific document
#   arn:cipherlake:vector:::*                        - All indexes
# ============================================================

# Admin always has access
allow {
    input.user.role == "admin"
}

# --- vector:Search ---

# Allow search if user has vector:Search permission
allow {
    input.action.type == "vector:Search"
    has_permission(input.user, "vector:Search")
}

# Allow search if user has s3:read on the bucket (backward compatible)
allow {
    input.action.type == "vector:Search"
    has_permission(input.user, "read")
    has_bucket_permission(input.user, input.object.bucket, "read")
}

# Allow search if user has vector:* permission
allow {
    input.action.type == "vector:Search"
    has_permission(input.user, "vector:*")
}

# --- vector:Index ---

# Allow indexing if user has vector:Index permission
allow {
    input.action.type == "vector:Index"
    has_permission(input.user, "vector:Index")
}

# Allow indexing if user has s3:write on the bucket (auto-index on upload)
allow {
    input.action.type == "vector:Index"
    has_permission(input.user, "write")
    has_bucket_permission(input.user, input.object.bucket, "write")
}

# --- vector:Delete ---

# Allow delete if user has vector:Delete permission
allow {
    input.action.type == "vector:Delete"
    has_permission(input.user, "vector:Delete")
}

# Allow delete if user has s3:delete on the bucket (auto-delete on object removal)
allow {
    input.action.type == "vector:Delete"
    has_permission(input.user, "delete")
    has_bucket_permission(input.user, input.object.bucket, "delete")
}

# --- vector:Manage ---

# Manage operations require explicit vector:Manage or admin
allow {
    input.action.type == "vector:Manage"
    has_permission(input.user, "vector:Manage")
}

allow {
    input.action.type == "vector:Manage"
    has_permission(input.user, "vector:*")
}

# --- Explicit Deny Rules ---

# Deny search on sensitive buckets for non-admin users
deny_sensitive {
    input.action.type == "vector:Search"
    sensitive_bucket(input.object.bucket)
    input.user.role != "admin"
}

# Deny indexing of binary content
deny_binary {
    input.action.type == "vector:Index"
    input.object.content_type != ""
    not is_text_content(input.object.content_type)
}

# --- Helpers ---

has_permission(user, perm) {
    perm in user.permissions
}

has_permission(user, perm) {
    "admin" in user.permissions
}

has_bucket_permission(user, bucket, perm) {
    perms := user.bucket_permissions[bucket]
    perm in perms
}

has_bucket_permission(user, bucket, perm) {
    perms := user.bucket_permissions["*"]
    perm in perms
}

has_bucket_permission(user, bucket, perm) {
    "*" in user.bucket_permissions[bucket]
}

sensitive_bucket(bucket) {
    bucket in ["secrets", "credentials", "private", "confidential"]
}

is_text_content(ct) {
    startswith(ct, "text/")
}

is_text_content(ct) {
    startswith(ct, "application/json")
}

is_text_content(ct) {
    startswith(ct, "application/xml")
}

# Reason for denial
reason = sprintf("user %s does not have %s permission on vector index %s", [input.user.id, input.action.type, input.object.bucket]) {
    not allow
}

# Whether auto-indexing is allowed for this bucket/content-type
auto_index_allowed {
    not sensitive_bucket(input.object.bucket)
    input.object.content_type == ""
}

auto_index_allowed {
    not sensitive_bucket(input.object.bucket)
    is_text_content(input.object.content_type)
}
