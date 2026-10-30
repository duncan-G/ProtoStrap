# The blob store (README.md#managed) is the application's files bucket. The database keeps a
# file's key, and a job carries the key, never the file. Users upload and download large
# files straight from the bucket with signed URLs, so it must not require the S3 endpoint,
# and browsers on the application's hosts may upload to it. Replicas read its name from
# FILES_BUCKET; only components that use files may touch it. The application's other
# buckets are in buckets.tf.

locals {
  # These are the origins whose browsers may use signed URLs on files and on every bucket
  # with browser set.
  cors_origins = concat([for h in local.hosts : "https://${h}"], var.cors_origins)
}

resource "aws_s3_bucket" "files" {
  bucket        = "${local.bucket_prefix}-files"
  force_destroy = var.force_destroy
  tags          = local.tags
}

resource "aws_s3_bucket_public_access_block" "files" {
  bucket                  = aws_s3_bucket.files.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "files" {
  bucket = aws_s3_bucket.files.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "files" {
  bucket = aws_s3_bucket.files.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# Signed URLs are https, so refusing plain HTTP costs replicas and browsers nothing. A
# policy that only denies isn't public, so the public-access block allows it.
resource "aws_s3_bucket_policy" "files" {
  bucket = aws_s3_bucket.files.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "TLSOnly"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource  = [aws_s3_bucket.files.arn, "${aws_s3_bucket.files.arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })

  # S3 refuses a bucket policy while the public-access block is still being applied.
  depends_on = [aws_s3_bucket_public_access_block.files]
}

resource "aws_s3_bucket_cors_configuration" "files" {
  count  = length(local.cors_origins) > 0 ? 1 : 0
  bucket = aws_s3_bucket.files.id
  cors_rule {
    allowed_methods = ["GET", "PUT", "POST", "HEAD"]
    allowed_origins = local.cors_origins
    allowed_headers = ["*"]
    expose_headers  = ["ETag"]
    max_age_seconds = 3600
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "files" {
  bucket = aws_s3_bucket.files.id
  rule {
    id     = "abandoned-uploads"
    status = "Enabled"
    filter {}
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}
