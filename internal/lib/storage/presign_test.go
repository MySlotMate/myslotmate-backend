package storage

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestSignPrivateURL_RewritesOnlyPrivatePrefix(t *testing.T) {
	c := s3.New(s3.Options{Region: "ap-south-1", Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AK", "SK", ""))})
	s := NewUploadService(c, "myslotmates3", "ap-south-1")
	base := "https://myslotmates3.s3.ap-south-1.amazonaws.com/"

	got := s.SignPrivateURL(base + "hosts/government-ids/abc_id.png")
	if !strings.Contains(got, "X-Amz-Signature=") || !strings.Contains(got, "hosts/government-ids/abc_id.png") || !strings.Contains(got, "X-Amz-Expires=900") {
		t.Fatalf("private document not presigned for 15 min: %s", got)
	}
	for _, u := range []string{
		base + "general/p.png",
		"https://myslotmate.s3.us-east-2.amazonaws.com/hosts/government-ids/old.png",
		"",
	} {
		if got := s.SignPrivateURL(u); got != u {
			t.Errorf("%q rewritten to %q", u, got)
		}
	}
}
