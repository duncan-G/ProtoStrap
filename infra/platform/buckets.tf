# The application's own buckets (docs/protostrap.md#spec, "Buckets"), besides the files
# bucket: one per bucket in the spec, private, encrypted and reached only over TLS, with the
# browser access, versioning, object lock and expiry the spec gives it. Replicas read a
# bucket's name from BUCKET_<NAME>; only components that use it may touch it
# (components.tf).
#
# A bucket removed from the spec is kept, as it was, while its settings are in
# retired_buckets. They are needed because S3 can't stop versioning a locked bucket.

resource "aws_s3_bucket" "app" {
  for_each      = local.kept_buckets
  bucket        = "${local.bucket_prefix}-${each.key}"
  force_destroy = var.force_destroy
  tags          = merge(local.tags, { "protostrap:bucket" = each.key })
}

resource "aws_s3_bucket_public_access_block" "app" {
  for_each                = aws_s3_bucket.app
  bucket                  = each.value.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "app" {
  for_each = aws_s3_bucket.app
  bucket   = each.value.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

# SSE-S3, not KMS: a browser's upload with a signed URL or POST is authorized by the
# signature alone, and a KMS key would need a grant in every role that signs one.
resource "aws_s3_bucket_server_side_encryption_configuration" "app" {
  for_each = aws_s3_bucket.app
  bucket   = each.value.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# As for files: signed URLs are https, and a policy that only denies isn't public.
resource "aws_s3_bucket_policy" "app" {
  for_each = aws_s3_bucket.app
  bucket   = each.value.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "TLSOnly"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource  = [each.value.arn, "${each.value.arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })

  depends_on = [aws_s3_bucket_public_access_block.app]
}

resource "aws_s3_bucket_versioning" "app" {
  for_each = { for name, b in local.kept_buckets : name => b if b.versions != null }
  bucket   = aws_s3_bucket.app[each.key].id
  versioning_configuration {
    status = "Enabled"
  }
}

# Object lock with no default retention, so an object is locked only when a replica writes
# it under GOVERNANCE retention. S3 locks only a versioned bucket, and once on, object lock
# can't be turned off.
resource "aws_s3_bucket_object_lock_configuration" "app" {
  for_each            = { for name, b in local.kept_buckets : name => b if b.lock }
  bucket              = aws_s3_bucket.app[each.key].id
  object_lock_enabled = "Enabled"

  depends_on = [aws_s3_bucket_versioning.app]
}

resource "aws_s3_bucket_cors_configuration" "app" {
  for_each = { for name, b in local.kept_buckets : name => b if b.browser && length(local.cors_origins) > 0 }
  bucket   = aws_s3_bucket.app[each.key].id
  cors_rule {
    allowed_methods = ["GET", "PUT", "POST", "HEAD"]
    allowed_origins = local.cors_origins
    allowed_headers = ["*"]
    expose_headers  = ["ETag"]
    max_age_seconds = 3600
  }
}

# Every bucket drops uploads abandoned part way. Expire deletes objects that many days after
# they are written; in a versioned bucket that leaves a delete marker, and versions keeps the
# earlier version that many days more. A version still under retention waits for it to end.
resource "aws_s3_bucket_lifecycle_configuration" "app" {
  for_each = aws_s3_bucket.app
  bucket   = each.value.id

  rule {
    id     = "abandoned-uploads"
    status = "Enabled"
    filter {}
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  dynamic "rule" {
    for_each = local.kept_buckets[each.key].expire == null ? [] : [local.kept_buckets[each.key].expire]
    content {
      id     = "expire"
      status = "Enabled"
      filter {}
      expiration {
        days = rule.value
      }
    }
  }

  dynamic "rule" {
    for_each = local.kept_buckets[each.key].versions == null ? [] : [local.kept_buckets[each.key].versions]
    content {
      id     = "old-versions"
      status = "Enabled"
      filter {}
      noncurrent_version_expiration {
        noncurrent_days = rule.value
      }
      expiration {
        expired_object_delete_marker = true
      }
    }
  }

  depends_on = [aws_s3_bucket_versioning.app]
}
