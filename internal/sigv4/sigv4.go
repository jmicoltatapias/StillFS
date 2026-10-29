// Package sigv4 validates the Authorization-header, single-chunk AWS
// Signature Version 4 mode used by the initial S3 API.
package sigv4

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const algorithm = "AWS4-HMAC-SHA256"

var emptyPayloadHash = sha256.Sum256(nil)
var ErrUnknownCredential = errors.New("unknown access key ID")

type Credential struct {
	Secret      string
	PrincipalID int64
	Active      bool
}

type Resolver interface {
	Resolve(context.Context, string) (Credential, error)
}

type Authenticator struct {
	Region       string
	MaxClockSkew time.Duration
	Resolver     Resolver
	Now          func() time.Time
}

type Authenticated struct {
	PrincipalID int64
	PayloadHash string
}

type Error struct {
	Code   string
	Status int
	Cause  error
}

func (e *Error) Error() string {
	if e.Cause == nil {
		return e.Code
	}
	return e.Code + ": " + e.Cause.Error()
}

func authError(code string, status int, cause error) error {
	return &Error{Code: code, Status: status, Cause: cause}
}

// Authenticate validates an Authorization-header signature and returns the
// authenticated principal plus the payload digest that a PUT handler must
// verify while streaming the body.
func (a Authenticator) Authenticate(ctx context.Context, request *http.Request) (Authenticated, error) {
	if request.Header.Get("Authorization") == "" {
		return Authenticated{}, authError("AccessDenied", http.StatusForbidden, nil)
	}
	if len(request.Header.Values("Authorization")) != 1 || len(request.Header.Values("X-Amz-Date")) != 1 || len(request.Header.Values("X-Amz-Content-Sha256")) != 1 {
		return Authenticated{}, authError("AuthorizationHeaderMalformed", http.StatusBadRequest, errors.New("authorization, x-amz-date, and payload hash must each occur once"))
	}
	parsed, err := parseAuthorization(request.Header.Get("Authorization"))
	if err != nil {
		return Authenticated{}, authError("AuthorizationHeaderMalformed", http.StatusBadRequest, err)
	}
	if parsed.algorithm != algorithm {
		return Authenticated{}, authError("AuthorizationHeaderMalformed", http.StatusBadRequest, errors.New("unsupported signature algorithm"))
	}
	if hasPresignParameters(request.URL) || request.Header.Get("X-Amz-Security-Token") != "" {
		return Authenticated{}, authError("InvalidRequest", http.StatusBadRequest, errors.New("presigned and session-token requests are unsupported"))
	}
	scope := strings.Split(parsed.credentialScope, "/")
	if len(scope) != 4 || scope[3] != "aws4_request" || scope[2] != "s3" || scope[1] != a.Region {
		return Authenticated{}, authError("AuthorizationHeaderMalformed", http.StatusBadRequest, errors.New("unsupported credential scope"))
	}
	dateValue := request.Header.Get("X-Amz-Date")
	if len(dateValue) != 16 {
		return Authenticated{}, authError("AuthorizationHeaderMalformed", http.StatusBadRequest, errors.New("x-amz-date is required"))
	}
	requestTime, err := time.Parse("20060102T150405Z", dateValue)
	if err != nil || scope[0] != dateValue[:8] {
		return Authenticated{}, authError("AuthorizationHeaderMalformed", http.StatusBadRequest, errors.New("invalid request date or credential scope date"))
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	if delta := now().Sub(requestTime); delta > a.MaxClockSkew || delta < -a.MaxClockSkew {
		return Authenticated{}, authError("RequestTimeTooSkewed", http.StatusForbidden, errors.New("request timestamp is outside the configured tolerance"))
	}
	payloadHash := request.Header.Get("X-Amz-Content-Sha256")
	if !validPayloadHash(payloadHash) {
		return Authenticated{}, authError("InvalidRequest", http.StatusBadRequest, errors.New("only a hexadecimal SHA-256 payload hash is supported"))
	}
	if request.Method != http.MethodPut && payloadHash != hex.EncodeToString(emptyPayloadHash[:]) {
		return Authenticated{}, authError("InvalidRequest", http.StatusBadRequest, errors.New("non-empty payload hashes are supported only for PUT"))
	}
	if request.Method != http.MethodPut && (request.ContentLength > 0 || len(request.TransferEncoding) > 0) {
		return Authenticated{}, authError("InvalidRequest", http.StatusBadRequest, errors.New("request bodies are supported only for PUT object"))
	}
	if !containsSorted(parsed.signedHeaders, "host") || !containsSorted(parsed.signedHeaders, "x-amz-date") {
		return Authenticated{}, authError("AuthorizationHeaderMalformed", http.StatusBadRequest, errors.New("host and x-amz-date must be signed"))
	}
	for name := range request.Header {
		lower := strings.ToLower(name)
		mustSign := lower == "content-type" || lower == "content-md5" || (strings.HasPrefix(lower, "x-amz-") && lower != "x-amz-date" && lower != "x-amz-content-sha256")
		if mustSign && !containsSorted(parsed.signedHeaders, lower) {
			return Authenticated{}, authError("AuthorizationHeaderMalformed", http.StatusBadRequest, fmt.Errorf("header %q must be signed", lower))
		}
	}
	for _, name := range parsed.signedHeaders {
		if name == "authorization" || name == "transfer-encoding" {
			return Authenticated{}, authError("AuthorizationHeaderMalformed", http.StatusBadRequest, errors.New("unsupported signed header"))
		}
	}
	canonical, err := CanonicalRequest(request, parsed.signedHeaders, payloadHash)
	if err != nil {
		return Authenticated{}, authError("AuthorizationHeaderMalformed", http.StatusBadRequest, err)
	}
	if a.Resolver == nil {
		return Authenticated{}, authError("InternalError", http.StatusInternalServerError, errors.New("credential resolver is unavailable"))
	}
	credential, err := a.Resolver.Resolve(ctx, parsed.accessKeyID)
	if err != nil {
		if errors.Is(err, ErrUnknownCredential) {
			return Authenticated{}, authError("InvalidAccessKeyId", http.StatusForbidden, nil)
		}
		return Authenticated{}, authError("InternalError", http.StatusInternalServerError, err)
	}
	if !credential.Active {
		return Authenticated{}, authError("AccessDenied", http.StatusForbidden, nil)
	}
	if credential.PrincipalID <= 0 || credential.Secret == "" {
		return Authenticated{}, authError("InternalError", http.StatusInternalServerError, errors.New("credential record is invalid"))
	}
	expected := Signature(credential.Secret, dateValue, scope[1], scope[2], canonical)
	provided, err := hex.DecodeString(parsed.signature)
	if err != nil || len(provided) != sha256.Size {
		return Authenticated{}, authError("SignatureDoesNotMatch", http.StatusForbidden, nil)
	}
	want, _ := hex.DecodeString(expected)
	if subtle.ConstantTimeCompare(provided, want) != 1 {
		return Authenticated{}, authError("SignatureDoesNotMatch", http.StatusForbidden, nil)
	}
	return Authenticated{PrincipalID: credential.PrincipalID, PayloadHash: payloadHash}, nil
}

type authorization struct {
	algorithm       string
	credentialScope string
	accessKeyID     string
	signedHeaders   []string
	signature       string
}

func parseAuthorization(value string) (authorization, error) {
	parts := strings.SplitN(value, " ", 2)
	if len(parts) != 2 {
		return authorization{}, errors.New("missing Authorization fields")
	}
	parsed := authorization{algorithm: parts[0]}
	fields := make(map[string]string)
	for _, field := range strings.Split(parts[1], ",") {
		pair := strings.SplitN(strings.TrimSpace(field), "=", 2)
		if len(pair) != 2 || pair[0] == "" || pair[1] == "" {
			return authorization{}, errors.New("malformed Authorization field")
		}
		if _, exists := fields[pair[0]]; exists {
			return authorization{}, errors.New("duplicate Authorization field")
		}
		fields[pair[0]] = pair[1]
	}
	credentialValue := fields["Credential"]
	scope := strings.Split(credentialValue, "/")
	if len(scope) != 5 || scope[0] == "" {
		return authorization{}, errors.New("malformed Credential scope")
	}
	parsed.accessKeyID = scope[0]
	parsed.credentialScope = strings.Join(scope[1:], "/")
	parsed.signedHeaders = strings.Split(fields["SignedHeaders"], ";")
	parsed.signature = fields["Signature"]
	if len(fields) != 3 || !validSignedHeaders(parsed.signedHeaders) || len(parsed.signature) != 64 {
		return authorization{}, errors.New("malformed signed headers or signature")
	}
	for _, char := range parsed.signature {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return authorization{}, errors.New("signature must be lowercase hexadecimal")
		}
	}
	return parsed, nil
}

func validSignedHeaders(headers []string) bool {
	if len(headers) == 0 {
		return false
	}
	for i, name := range headers {
		if name == "" || name != strings.ToLower(name) || strings.ContainsAny(name, " \t\r\n:;") {
			return false
		}
		if i > 0 && headers[i-1] >= name {
			return false
		}
	}
	return true
}

func containsSorted(items []string, value string) bool {
	i := sort.SearchStrings(items, value)
	return i < len(items) && items[i] == value
}

func validPayloadHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func hasPresignParameters(u *url.URL) bool {
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return true
	}
	for key := range query {
		if strings.HasPrefix(strings.ToLower(key), "x-amz-") && (strings.EqualFold(key, "X-Amz-Algorithm") || strings.EqualFold(key, "X-Amz-Signature")) {
			return true
		}
	}
	return false
}

// CanonicalRequest implements S3's non-normalizing path canonicalization and
// post-encoding query sorting.
func CanonicalRequest(request *http.Request, signedHeaders []string, payloadHash string) (string, error) {
	path, err := url.PathUnescape(request.URL.EscapedPath())
	if err != nil {
		return "", fmt.Errorf("decode request path: %w", err)
	}
	if path == "" {
		path = "/"
	}
	canonicalURI := uriEncode(path, false)
	canonicalQuery, err := canonicalizeQuery(request.URL.RawQuery)
	if err != nil {
		return "", err
	}
	var headerBuilder strings.Builder
	for _, name := range signedHeaders {
		value, ok := headerValue(request, name)
		if !ok {
			return "", fmt.Errorf("signed header %q is missing", name)
		}
		headerBuilder.WriteString(name)
		headerBuilder.WriteByte(':')
		headerBuilder.WriteString(normalizeHeader(value))
		headerBuilder.WriteByte('\n')
	}
	return request.Method + "\n" + canonicalURI + "\n" + canonicalQuery + "\n" + headerBuilder.String() + "\n" + strings.Join(signedHeaders, ";") + "\n" + payloadHash, nil
}

func headerValue(request *http.Request, name string) (string, bool) {
	if name == "host" {
		if request.Host == "" {
			return "", false
		}
		return request.Host, true
	}
	if name == "content-length" {
		if request.ContentLength < 0 {
			return "", false
		}
		return strconv.FormatInt(request.ContentLength, 10), true
	}
	values := request.Header.Values(http.CanonicalHeaderKey(name))
	if len(values) == 0 {
		return "", false
	}
	return strings.Join(values, ","), true
}

func normalizeHeader(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func canonicalizeQuery(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	type pair struct{ key, value string }
	pairs := make([]pair, 0)
	for _, item := range strings.Split(raw, "&") {
		if item == "" {
			continue
		}
		keyValue := strings.SplitN(item, "=", 2)
		key, err := url.QueryUnescape(keyValue[0])
		if err != nil {
			return "", fmt.Errorf("decode query key: %w", err)
		}
		value := ""
		if len(keyValue) == 2 {
			value, err = url.QueryUnescape(keyValue[1])
			if err != nil {
				return "", fmt.Errorf("decode query value: %w", err)
			}
		}
		pairs = append(pairs, pair{uriEncode(key, true), uriEncode(value, true)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key == pairs[j].key {
			return pairs[i].value < pairs[j].value
		}
		return pairs[i].key < pairs[j].key
	})
	encoded := make([]string, len(pairs))
	for i, item := range pairs {
		encoded[i] = item.key + "=" + item.value
	}
	return strings.Join(encoded, "&"), nil
}

func uriEncode(value string, encodeSlash bool) string {
	const hexUpper = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '-' || b == '_' || b == '.' || b == '~' || (b == '/' && !encodeSlash) {
			out.WriteByte(b)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(hexUpper[b>>4])
		out.WriteByte(hexUpper[b&0x0f])
	}
	return out.String()
}

// Signature derives the AWS4 signing key and returns the lowercase signature.
func Signature(secret, timestamp, region, service, canonicalRequest string) string {
	canonicalHash := sha256.Sum256([]byte(canonicalRequest))
	date := timestamp[:8]
	stringToSign := algorithm + "\n" + timestamp + "\n" + date + "/" + region + "/" + service + "/aws4_request\n" + hex.EncodeToString(canonicalHash[:])
	dateKey := hmacSHA256([]byte("AWS4"+secret), date)
	regionKey := hmacSHA256(dateKey, region)
	serviceKey := hmacSHA256(regionKey, service)
	signingKey := hmacSHA256(serviceKey, "aws4_request")
	return hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
}

func hmacSHA256(key []byte, message string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(message))
	return h.Sum(nil)
}
