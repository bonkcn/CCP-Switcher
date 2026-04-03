package sync

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// S3 implements Backend using S3-compatible storage with AWS Signature V4.
// No external SDK dependency — uses raw HTTP with manual signing.
type S3 struct {
	endpoint  string
	bucket    string
	region    string
	filePath  string
	accessKey string
	secretKey string
	client    *http.Client
}

// NewS3 creates a new S3-compatible backend.
func NewS3(cfg Config) *S3 {
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = "us-east-1"
	}
	filePath := strings.TrimSpace(cfg.Path)
	if filePath == "" {
		filePath = DefaultPath
	}
	if !strings.HasPrefix(filePath, "/") {
		filePath = "/" + filePath
	}

	return &S3{
		endpoint:  endpoint,
		bucket:    strings.TrimSpace(cfg.Bucket),
		region:    region,
		filePath:  filePath,
		accessKey: cfg.KeyID,
		secretKey: cfg.Secret,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *S3) objectURL() string {
	if s.endpoint != "" {
		return s.endpoint + "/" + s.bucket + s.filePath
	}
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com%s", s.bucket, s.region, s.filePath)
}

func (s *S3) hostHeader() string {
	if s.endpoint != "" {
		// Extract host from endpoint
		u := s.endpoint
		u = strings.TrimPrefix(u, "https://")
		u = strings.TrimPrefix(u, "http://")
		if idx := strings.Index(u, "/"); idx >= 0 {
			u = u[:idx]
		}
		return u
	}
	return fmt.Sprintf("%s.s3.%s.amazonaws.com", s.bucket, s.region)
}

func (s *S3) Push(ctx context.Context, data []byte) error {
	url := s.objectURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("s3 push: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	s.signRequest(req, data)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("s3 push: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("s3 push: HTTP %d %s", resp.StatusCode, resp.Status)
}

func (s *S3) Pull(ctx context.Context) ([]byte, error) {
	url := s.objectURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("s3 pull: %w", err)
	}
	s.signRequest(req, nil)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3 pull: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("s3 pull: 远程文件不存在或无权访问 (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("s3 pull: HTTP %d %s", resp.StatusCode, resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("s3 pull: %w", err)
	}
	return data, nil
}

func (s *S3) Test(ctx context.Context) error {
	// HEAD on the bucket root to verify access
	var url string
	if s.endpoint != "" {
		url = s.endpoint + "/" + s.bucket + "/"
	} else {
		url = fmt.Sprintf("https://%s.s3.%s.amazonaws.com/", s.bucket, s.region)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return fmt.Errorf("s3 test: %w", err)
	}
	s.signRequest(req, nil)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("s3 test: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("s3 test: 认证失败 (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode == 404 {
		return fmt.Errorf("s3 test: Bucket 不存在 (HTTP 404)")
	}
	return fmt.Errorf("s3 test: HTTP %d %s", resp.StatusCode, resp.Status)
}

// signRequest adds AWS Signature V4 headers to the request.
func (s *S3) signRequest(req *http.Request, payload []byte) {
	now := time.Now().UTC()
	datestamp := now.Format("20060102")
	amzdate := now.Format("20060102T150405Z")

	payloadHash := sha256Hex(payload)

	req.Header.Set("Host", s.hostHeader())
	req.Header.Set("X-Amz-Date", amzdate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	// Canonical request
	signedHeaders, canonicalHeaders := s.canonicalHeaders(req)
	canonicalURI := req.URL.Path
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQueryString := req.URL.RawQuery

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	// String to sign
	credentialScope := datestamp + "/" + s.region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzdate + "\n" + credentialScope + "\n" + sha256Hex([]byte(canonicalRequest))

	// Signing key
	signingKey := s.deriveSigningKey(datestamp)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	// Authorization header
	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKey, credentialScope, signedHeaders, signature)
	req.Header.Set("Authorization", auth)
}

func (s *S3) canonicalHeaders(req *http.Request) (signedHeaders string, canonicalHeaders string) {
	headers := make(map[string]string)
	var keys []string

	for key := range req.Header {
		lower := strings.ToLower(key)
		if lower == "host" || lower == "content-type" || strings.HasPrefix(lower, "x-amz-") {
			headers[lower] = strings.TrimSpace(req.Header.Get(key))
			keys = append(keys, lower)
		}
	}
	sort.Strings(keys)

	var canonical strings.Builder
	for _, k := range keys {
		canonical.WriteString(k + ":" + headers[k] + "\n")
	}

	return strings.Join(keys, ";"), canonical.String()
}

func (s *S3) deriveSigningKey(datestamp string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+s.secretKey), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(s.region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return kSigning
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}
