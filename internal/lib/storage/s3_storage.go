package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

// Allowed MIME types and max file size for uploads.
var (
	AllowedMIMETypes = map[string]bool{
		"image/svg+xml": true,
		"image/png":     true,
		"image/jpeg":    true,
		// The Android picker hands back webp, so the mobile app would otherwise
		// fail on perfectly good photos. Kept in step with the Node backend's
		// allowlist — both write to the same bucket.
		"image/webp":      true,
		"application/pdf": true,
	}
	MaxFileSize int64 = 10 * 1024 * 1024 // 10 MB
)

// UploadService provides file upload capability to AWS S3.
type UploadService struct {
	client *s3.Client
	bucket string
	region string
}

// NewUploadService creates a new UploadService backed by AWS S3.
func NewUploadService(client *s3.Client, bucket, region string) *UploadService {
	return &UploadService{client: client, bucket: bucket, region: region}
}

// UploadResult holds the outcome of a single file upload.
type UploadResult struct {
	FileName string `json:"file_name"`
	URL      string `json:"url"`
	Size     int64  `json:"size"`
}

// UploadFile validates and uploads a single multipart file to S3.
// folder is the S3 key prefix, e.g. "support/evidence".
func (s *UploadService) UploadFile(ctx context.Context, folder string, fh *multipart.FileHeader) (*UploadResult, error) {
	if fh.Size > MaxFileSize {
		return nil, fmt.Errorf("file %q exceeds maximum size of 10 MB", fh.Filename)
	}

	contentType := fh.Header.Get("Content-Type")
	if !AllowedMIMETypes[contentType] {
		// fallback: check extension
		ext := strings.ToLower(filepath.Ext(fh.Filename))
		switch ext {
		case ".svg":
			contentType = "image/svg+xml"
		case ".png":
			contentType = "image/png"
		case ".jpg", ".jpeg":
			contentType = "image/jpeg"
		case ".webp":
			contentType = "image/webp"
		case ".pdf":
			contentType = "application/pdf"
		default:
			return nil, fmt.Errorf("file type %q is not allowed; accepted: SVG, PNG, JPG, WEBP, PDF", ext)
		}
	}

	f, err := fh.Open()
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	objectKey := fmt.Sprintf("%s/%s_%s", folder, uuid.New().String(), sanitizeFilename(fh.Filename))

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// Read file content for Content-Length
	body := io.Reader(f)

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(objectKey),
		Body:         body,
		ContentType:  aws.String(contentType),
		CacheControl: aws.String(cacheControl(folder)),
	})
	if err != nil {
		return nil, fmt.Errorf("upload to S3: %w", err)
	}

	// Build the public URL
	publicURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", s.bucket, s.region, objectKey)

	return &UploadResult{
		FileName: fh.Filename,
		URL:      publicURL,
		Size:     fh.Size,
	}, nil
}

// UploadFromURL downloads an image from a remote URL and re-hosts it on S3.
// Used to persist third-party media (e.g. Instagram CDN images, whose URLs
// expire) under our own bucket. Only image content types are accepted.
func (s *UploadService) UploadFromURL(ctx context.Context, folder, srcURL, baseName string) (*UploadResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", srcURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: status %d", srcURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read download: %w", err)
	}
	if int64(len(body)) > MaxFileSize {
		return nil, fmt.Errorf("remote file exceeds maximum size of 10 MB")
	}

	contentType := resp.Header.Get("Content-Type")
	if i := strings.Index(contentType, ";"); i >= 0 {
		contentType = strings.TrimSpace(contentType[:i])
	}
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	ext, ok := map[string]string{
		"image/jpeg": ".jpg",
		"image/png":  ".png",
		"image/webp": ".webp",
	}[contentType]
	if !ok {
		return nil, fmt.Errorf("remote content type %q is not an accepted image type", contentType)
	}

	fileName := sanitizeFilename(baseName) + ext
	objectKey := fmt.Sprintf("%s/%s_%s", folder, uuid.New().String(), fileName)

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(objectKey),
		Body:         bytes.NewReader(body),
		ContentType:  aws.String(contentType),
		CacheControl: aws.String("public, max-age=86400"),
	})
	if err != nil {
		return nil, fmt.Errorf("upload to S3: %w", err)
	}

	return &UploadResult{
		FileName: fileName,
		URL:      fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", s.bucket, s.region, objectKey),
		Size:     int64(len(body)),
	}, nil
}

// UploadMultiple uploads several files and returns all URLs.
func (s *UploadService) UploadMultiple(ctx context.Context, folder string, files []*multipart.FileHeader) ([]UploadResult, error) {
	var results []UploadResult
	for _, fh := range files {
		res, err := s.UploadFile(ctx, folder, fh)
		if err != nil {
			return nil, err
		}
		results = append(results, *res)
	}
	return results, nil
}

// sanitizeFilename replaces spaces and keeps the name URL-safe.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, " ", "_")
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, name)
	return name
}

// PublicURLPrefix is the prefix of every URL this service hands out,
// "https://<bucket>.s3.<region>.amazonaws.com/".
func (s *UploadService) PublicURLPrefix() string {
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/", s.bucket, s.region)
}

// SignPrivateURL returns a 15-minute signed GET link for a URL in this
// bucket's private documents folder (hosts/government-ids/). Any other URL,
// including other buckets, comes back unchanged. Signing is local; no network.
func (s *UploadService) SignPrivateURL(u string) string {
	key, ok := strings.CutPrefix(u, s.PublicURLPrefix())
	if !ok || !strings.HasPrefix(key, privateDocsFolder) || strings.Contains(key, "..") {
		return u
	}
	req, err := s3.NewPresignClient(s.client).PresignGetObject(context.Background(),
		&s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)},
		s3.WithPresignExpires(15*time.Minute))
	if err != nil {
		return u
	}
	return req.URL
}

// privateDocsFolder mirrors models.PrivateDocsFolder (storage can't import models).
const privateDocsFolder = "hosts/government-ids/"

// DeleteOwnedObject deletes the object behind publicURL, but only when that URL
// points into this bucket under folder/ — anything else (other folders, other
// hosts) is left alone, since callers pass URLs that users supplied.
// Returns (false, nil) when the URL was not ours to delete.
func (s *UploadService) DeleteOwnedObject(ctx context.Context, folder, publicURL string) (bool, error) {
	key, ok := strings.CutPrefix(publicURL, s.PublicURLPrefix())
	if !ok || !strings.HasPrefix(key, folder+"/") || strings.Contains(key, "..") {
		return false, nil
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err == nil, err
}

// cacheControl keeps private documents out of shared and browser caches.
func cacheControl(folder string) string {
	if strings.HasPrefix(folder+"/", privateDocsFolder) {
		return "private, no-store"
	}
	return "public, max-age=86400"
}

// billingFolder mirrors models.BillingDocsFolder: archived invoice PDFs.
const billingFolder = "billing/invoices/"

// PutPrivateObject stores a generated document under billing/invoices/,
// private and never cached. Other keys are refused.
func (s *UploadService) PutPrivateObject(ctx context.Context, key, contentType string, body []byte) error {
	if !strings.HasPrefix(key, billingFolder) || strings.Contains(key, "..") {
		return fmt.Errorf("refusing to write %q outside %s", key, billingFolder)
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(key),
		Body:         bytes.NewReader(body),
		ContentType:  aws.String(contentType),
		CacheControl: aws.String("private, no-store"),
	})
	return err
}

// GetPrivateObject reads a document stored with PutPrivateObject.
func (s *UploadService) GetPrivateObject(ctx context.Context, key string) ([]byte, error) {
	if !strings.HasPrefix(key, billingFolder) || strings.Contains(key, "..") {
		return nil, fmt.Errorf("refusing to read %q outside %s", key, billingFolder)
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// boostFolder holds Boost creatives and reports: private, read through
// short-lived signed links, uploaded straight from the browser.
const boostFolder = "boosts/"

func boostKeyOK(key string) bool {
	return strings.HasPrefix(key, boostFolder) && !strings.Contains(key, "..")
}

// PresignBoostUpload returns a signed PUT URL for one file under boosts/.
// The content type and length are signed, so the browser must send exactly
// those (the bucket's CORS must allow PUT from the website).
func (s *UploadService) PresignBoostUpload(ctx context.Context, key, contentType string, size int64) (string, error) {
	if !boostKeyOK(key) {
		return "", fmt.Errorf("refusing to sign %q outside %s", key, boostFolder)
	}
	req, err := s3.NewPresignClient(s.client).PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(size),
		CacheControl:  aws.String("private, no-store"),
	}, s3.WithPresignExpires(30*time.Minute))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// SignBoostDownload returns a one-hour signed GET link for a file under
// boosts/, served as an attachment named name.
func (s *UploadService) SignBoostDownload(key, name string) string {
	if !boostKeyOK(key) {
		return ""
	}
	in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if name != "" {
		in.ResponseContentDisposition = aws.String(fmt.Sprintf("inline; filename=%q", sanitizeFilename(name)))
	}
	req, err := s3.NewPresignClient(s.client).PresignGetObject(context.Background(), in, s3.WithPresignExpires(time.Hour))
	if err != nil {
		return ""
	}
	return req.URL
}

// HeadBoostObject returns the size and content type of an uploaded file
// under boosts/, or an error when it isn't there.
func (s *UploadService) HeadBoostObject(ctx context.Context, key string) (int64, string, error) {
	if !boostKeyOK(key) {
		return 0, "", fmt.Errorf("refusing to read %q outside %s", key, boostFolder)
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return 0, "", err
	}
	return aws.ToInt64(out.ContentLength), aws.ToString(out.ContentType), nil
}
