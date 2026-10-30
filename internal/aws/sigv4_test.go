package aws

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// These vectors come from the AWS documentation and were checked independently with openssl.

func TestSignIAMListUsersExample(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	creds := Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

	Sign(req, EmptyPayloadHash, creds, "us-east-1", "iam", now)

	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date, " +
		"Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization\n got %s\nwant %s", got, want)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Fatalf("X-Amz-Date = %q", got)
	}
}

func TestSigningKeyExample(t *testing.T) {
	key := signingKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20150830", "us-east-1", "iam")
	if got := hexString(key); got != "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9" {
		t.Fatalf("signing key %s", got)
	}
}

func TestSignS3GetObjectExample(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	req.Header.Set("X-Amz-Content-Sha256", EmptyPayloadHash)
	creds := Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}

	Sign(req, EmptyPayloadHash, creds, "us-east-1", "s3", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))

	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization\n got %s\nwant %s", got, want)
	}
}

func TestSignAddsSessionToken(t *testing.T) {
	req, _ := http.NewRequest("POST", "https://sts.eu-west-1.amazonaws.com/", nil)
	creds := Credentials{AccessKeyID: "AK", SecretAccessKey: "SK", SessionToken: "TOKEN"}
	Sign(req, EmptyPayloadHash, creds, "eu-west-1", "sts", time.Now())
	if req.Header.Get("X-Amz-Security-Token") != "TOKEN" {
		t.Fatal("session token header missing")
	}
	if !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Fatalf("session token must be signed: %s", req.Header.Get("Authorization"))
	}
}

func TestCanonicalQuerySortsAndEncodes(t *testing.T) {
	got := canonicalQuery("b=2&a=hello world&a=x/y&c=~_.-")
	want := "a=hello%20world&a=x%2Fy&b=2&c=~_.-"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestCanonicalPathEncodesEachSegmentOnce(t *testing.T) {
	cases := map[string]string{
		"":                        "/",
		"/":                       "/",
		"/postgres/2026-10-24.gz": "/postgres/2026-10-24.gz",
		"/a b/c+d":                "/a%20b/c%2Bd",
		"/protostrap/1.4.0/x":     "/protostrap/1.4.0/x",
	}
	for in, want := range cases {
		if got := canonicalPath(in); got != want {
			t.Errorf("canonicalPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCredentialsExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := Credentials{AccessKeyID: "a", Expires: now.Add(10 * time.Minute)}
	if c.ExpiresWithin(now, 5*time.Minute) {
		t.Error("10 minutes left is not within 5")
	}
	if !c.ExpiresWithin(now, 15*time.Minute) {
		t.Error("10 minutes left is within 15")
	}
	if (Credentials{AccessKeyID: "a"}).ExpiresWithin(now, time.Hour) {
		t.Error("credentials without an expiry never expire")
	}
	if !(Credentials{}).ExpiresWithin(now, 0) {
		t.Error("empty credentials are always expired")
	}
}
