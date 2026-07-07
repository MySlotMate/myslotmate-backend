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
		"image/svg+xml":   true,
		"image/png":       true,
		"image/jpeg":      true,
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
		case ".pdf":
			contentType = "application/pdf"
		default:
			return nil, fmt.Errorf("file type %q is not allowed; accepted: SVG, PNG, JPG, PDF", ext)
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
		CacheControl: aws.String("public, max-age=86400"),
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
