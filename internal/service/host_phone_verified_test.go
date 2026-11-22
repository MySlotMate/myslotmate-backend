package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"myslotmate-backend/internal/repository"

	"github.com/google/uuid"
)

type fakePhoneUsers struct {
	repository.UserRepository
	at *time.Time
}

func (f fakePhoneUsers) PhoneVerifiedAt(ctx context.Context, id uuid.UUID) (*time.Time, error) {
	return f.at, nil
}

func TestCheckPhoneVerified(t *testing.T) {
	ago := func(d time.Duration) *time.Time { v := time.Now().Add(-d); return &v }
	cases := []struct {
		name     string
		hostType string
		at       *time.Time
		want     string
	}{
		{"verified just now", "community", ago(time.Minute), ""},
		{"never verified", "professional", nil, "phone_not_verified"},
		{"verified too long ago", "community", ago(20 * time.Minute), "phone_not_verified"},
		{"old app body without host_type is not checked", "", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &hostService{userRepo: fakePhoneUsers{at: c.at}}
			req := HostApplicationRequest{}
			req.HostType = c.hostType
			err := s.checkPhoneVerified(context.Background(), uuid.New(), req)
			got := ""
			var ae *ApplicationError
			if errors.As(err, &ae) {
				got = ae.Code
			} else if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
