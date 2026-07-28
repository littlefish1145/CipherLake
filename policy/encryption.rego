package cipherlake.encryption

# Default: encryption is required for all objects
default required = true

# Encryption required for sensitive buckets
required {
    sensitive_bucket(input.object.bucket)
}

# Encryption required for objects with sensitive metadata
required {
    input.object.metadata["sensitive"] == "true"
}

# Encryption required for objects larger than threshold (e.g., 1MB)
required {
    input.object.size > 1048576
}

# Encryption not required for public buckets
required = false {
    public_bucket(input.object.bucket)
}

# Encryption not required for small objects in non-sensitive buckets
required = false {
    not sensitive_bucket(input.object.bucket)
    input.object.size <= 1024
}

# Helper: define sensitive buckets
sensitive_bucket(bucket) {
    bucket in ["secrets", "credentials", "private", "confidential"]
}

# Helper: define public buckets
public_bucket(bucket) {
    bucket in ["public", "assets", "thumbnails"]
}

# Encryption algorithm recommendation
algorithm = "AES-256-GCM" {
    required
}

algorithm = "none" {
    not required
}