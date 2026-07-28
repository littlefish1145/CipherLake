package cipherlake.access

# Default deny
default allow = false

# Allow if user has admin role
allow {
    input.user.role == "admin"
}

# Allow read if user has read permission or bucket read permission
allow {
    input.action.type == "read"
    has_permission(input.user, "read")
}

allow {
    input.action.type == "read"
    has_bucket_permission(input.user, input.object.bucket, "read")
}

# Allow write if user has write permission or bucket write permission
allow {
    input.action.type == "write"
    has_permission(input.user, "write")
}

allow {
    input.action.type == "write"
    has_bucket_permission(input.user, input.object.bucket, "write")
}

# Allow delete if user has delete permission or bucket delete permission
allow {
    input.action.type == "delete"
    has_permission(input.user, "delete")
}

allow {
    input.action.type == "delete"
    has_bucket_permission(input.user, input.object.bucket, "delete")
}

# Allow list if user has read permission
allow {
    input.action.type == "list"
    has_permission(input.user, "read")
}

# Helper: check global permission
has_permission(user, perm) {
    perm in user.permissions
}

has_permission(user, perm) {
    "admin" in user.permissions
}

# Helper: check bucket-specific permission
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

# Reason for denial
reason = sprintf("user %s does not have %s permission on bucket %s", [input.user.id, input.action.type, input.object.bucket]) {
    not allow
}