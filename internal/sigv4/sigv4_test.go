package sigv4

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAWSGetObjectAuthorizationVector(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	request.Host = "examplebucket.s3.amazonaws.com"
	request.Header.Set("Range", "bytes=0-9")
	request.Header.Set("X-Amz-Content-Sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	request.Header.Set("X-Amz-Date", "20130524T000000Z")
	signed := []string{"host", "range", "x-amz-content-sha256", "x-amz-date"}
	canonical, err := CanonicalRequest(request, signed, request.Header.Get("X-Amz-Content-Sha256"))
	if err != nil {
		t.Fatal(err)
	}
	got := Signature("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "20130524T000000Z", "us-east-1", "s3", canonical)
	if got != "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41" {
		t.Fatalf("AWS SigV4 vector signature = %s", got)
	}
}

func TestCanonicalQueryAndEscapedPath(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/bucket/a%20b/%252F?z=+&prefix=a%2Fb&empty&dup=2&dup=1", nil)
	canonical, err := CanonicalRequest(request, []string{"host"}, "payload")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(canonical, "GET\n/bucket/a%20b/%252F\n") {
		t.Fatalf("canonical path not escaped as expected:\n%s", canonical)
	}
	if !strings.Contains(canonical, "dup=1&dup=2&empty=&prefix=a%2Fb&z=%20") {
		t.Fatalf("canonical query not sorted/encoded:\n%s", canonical)
	}
}

func TestAuthenticateValidInvalidRevokedAndExpired(t *testing.T) {
	const timestamp = "20130524T000000Z"
	resolver := &resolverStub{credential: Credential{Secret: "secret", PrincipalID: 7, Active: true}}
	request := signedRequest(t, timestamp, "secret")
	auth := Authenticator{Region: "us-east-1", MaxClockSkew: time.Minute, Resolver: resolver, Now: func() time.Time {
		value, _ := time.Parse("20060102T150405Z", timestamp)
		return value
	}}
	identity, err := auth.Authenticate(context.Background(), request)
	if err != nil || identity.PrincipalID != 7 {
		t.Fatalf("Authenticate() = (%#v, %v)", identity, err)
	}

	bad := request.Clone(context.Background())
	bad.Host = "tampered.example"
	if _, err := auth.Authenticate(context.Background(), bad); !authCode(err, "SignatureDoesNotMatch") {
		t.Fatalf("tampered request error = %v", err)
	}
	resolver.credential.Active = false
	if _, err := auth.Authenticate(context.Background(), request); !authCode(err, "AccessDenied") {
		t.Fatalf("revoked credential error = %v", err)
	}
	resolver.credential.Active = true
	resolver.err = ErrUnknownCredential
	if _, err := auth.Authenticate(context.Background(), request); !authCode(err, "InvalidAccessKeyId") {
		t.Fatalf("unknown credential error = %v", err)
	}
	resolver.err = nil
	tooOld := auth.Now().Add(-2 * time.Minute)
	request = signedRequest(t, tooOld.Format("20060102T150405Z"), "secret")
	if _, err := auth.Authenticate(context.Background(), request); !authCode(err, "RequestTimeTooSkewed") {
		t.Fatalf("old request error = %v", err)
	}
}

func TestAuthenticateRejectsUnsupportedModesAndMalformedFields(t *testing.T) {
	const timestamp = "20130524T000000Z"
	resolver := &resolverStub{credential: Credential{Secret: "secret", PrincipalID: 7, Active: true}}
	now, _ := time.Parse("20060102T150405Z", timestamp)
	auth := Authenticator{Region: "us-east-1", MaxClockSkew: time.Minute, Resolver: resolver, Now: func() time.Time { return now }}
	tests := []struct {
		name string
		code string
		edit func(*http.Request)
	}{
		{name: "unsigned payload", code: "InvalidRequest", edit: func(r *http.Request) { r.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD") }},
		{name: "session token", code: "InvalidRequest", edit: func(r *http.Request) { r.Header.Set("X-Amz-Security-Token", "test-token") }},
		{name: "presigned query", code: "InvalidRequest", edit: func(r *http.Request) { r.URL.RawQuery += "&X-Amz-Signature=abc" }},
		{name: "duplicate authorization", code: "AuthorizationHeaderMalformed", edit: func(r *http.Request) { r.Header.Add("Authorization", r.Header.Get("Authorization")) }},
		{name: "duplicate date", code: "AuthorizationHeaderMalformed", edit: func(r *http.Request) { r.Header.Add("X-Amz-Date", timestamp) }},
		{name: "wrong region", code: "AuthorizationHeaderMalformed", edit: func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "/us-east-1/s3/", "/us-west-2/s3/", 1))
		}},
		{name: "malformed signature hex", code: "AuthorizationHeaderMalformed", edit: func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "Signature=", "Signature=z", 1))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := signedRequest(t, timestamp, "secret").Clone(context.Background())
			test.edit(request)
			if _, err := auth.Authenticate(context.Background(), request); !authCode(err, test.code) {
				t.Fatalf("Authenticate() error = %v, want %s", err, test.code)
			}
		})
	}
}

func TestVerifiedBodyStreamsAndRejectsMismatch(t *testing.T) {
	content := strings.Repeat("stream-data", 10000)
	sum := sha256.Sum256([]byte(content))
	body, err := NewVerifiedBody(io.NopCloser(strings.NewReader(content)), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(body)
	if err != nil || string(read) != content || !body.Verified() {
		t.Fatalf("verified body read = %d bytes, err=%v, verified=%v", len(read), err, body.Verified())
	}
	wrongHash := sha256.Sum256([]byte("different"))
	body, err = NewVerifiedBody(io.NopCloser(strings.NewReader(content)), hex.EncodeToString(wrongHash[:]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, body); !errors.Is(err, ErrPayloadHashMismatch) || body.Verified() {
		t.Fatalf("mismatched body = err %v, verified %v", err, body.Verified())
	}
}

type resolverStub struct {
	credential Credential
	err        error
}

func (r *resolverStub) Resolve(context.Context, string) (Credential, error) {
	return r.credential, r.err
}

func signedRequest(t *testing.T, timestamp, secret string) *http.Request {
	t.Helper()
	bodyHash := hex.EncodeToString(emptyPayloadHash[:])
	request := httptest.NewRequest(http.MethodGet, "https://stillfs.test/bucket?prefix=a%2Fb", nil)
	request.Host = "stillfs.test"
	request.Header.Set("X-Amz-Date", timestamp)
	request.Header.Set("X-Amz-Content-Sha256", bodyHash)
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonical, err := CanonicalRequest(request, signed, bodyHash)
	if err != nil {
		t.Fatal(err)
	}
	date := timestamp[:8]
	signature := Signature(secret, timestamp, "us-east-1", "s3", canonical)
	request.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=AKID/%s/us-east-1/s3/aws4_request,SignedHeaders=%s,Signature=%s", date, strings.Join(signed, ";"), signature))
	return request
}

func authCode(err error, code string) bool {
	var authErr *Error
	return errors.As(err, &authErr) && authErr.Code == code
}
