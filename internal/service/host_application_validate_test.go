package service

import (
	"strings"
	"testing"
)

const testBucket = "https://b.s3.ap-south-1.amazonaws.com/"

func sp(s string) *string { return &s }

func validCommunity() HostApplicationRequest {
	r := HostApplicationRequest{
		FirstName:       "Asha",
		City:            "Guwahati",
		Description:     sp("We read one book a month and meet at a cafe."),
		GovernmentIDURL: sp(testBucket + "hosts/government-ids/id.png"),
	}
	r.HostType = "community"
	r.CommunityCategory = sp("social")
	return r
}

func validProfessional() HostApplicationRequest {
	r := HostApplicationRequest{
		FirstName:       "Asha",
		City:            "Guwahati",
		GovernmentIDURL: sp(testBucket + "hosts/government-ids/id.png"),
	}
	r.HostType = "professional"
	r.Profession = sp("yoga")
	r.YearsExperience = sp("3–5 years")
	r.Specialization = sp("Hatha yoga")
	r.LicenseNumber = sp("LIC-1")
	return r
}

func TestValidateHostApplication(t *testing.T) {
	cases := []struct {
		name string
		mod  func() HostApplicationRequest
		want string
	}{
		{"valid community", validCommunity, ""},
		{"valid professional", validProfessional, ""},
		{"legacy body without host_type is not checked", func() HostApplicationRequest {
			return HostApplicationRequest{FirstName: "A", City: "B"}
		}, ""},
		{"unknown host_type", func() HostApplicationRequest { r := validCommunity(); r.HostType = "vip"; return r }, "invalid_host_type"},
		{"no government id", func() HostApplicationRequest { r := validCommunity(); r.GovernmentIDURL = sp(" "); return r }, "government_id_required"},
		{"community no category", func() HostApplicationRequest { r := validCommunity(); r.CommunityCategory = nil; return r }, "category_required"},
		{"community unknown category", func() HostApplicationRequest { r := validCommunity(); r.CommunityCategory = sp("party"); return r }, "category_required"},
		{"community short description", func() HostApplicationRequest { r := validCommunity(); r.Description = sp("  short  "); return r }, "description_too_short"},
		{"professional unknown profession", func() HostApplicationRequest { r := validProfessional(); r.Profession = sp("astrology"); return r }, "profession_required"},
		{"professional no years", func() HostApplicationRequest { r := validProfessional(); r.YearsExperience = nil; return r }, "experience_required"},
		{"professional no specialization", func() HostApplicationRequest { r := validProfessional(); r.Specialization = sp(""); return r }, "experience_required"},
		{"professional no credential", func() HostApplicationRequest { r := validProfessional(); r.LicenseNumber = nil; return r }, "credential_required"},
		{"professional linkedin is a credential", func() HostApplicationRequest {
			r := validProfessional()
			r.LicenseNumber = nil
			r.SocialLinkedin = sp("https://linkedin.com/in/asha")
			return r
		}, ""},
		{"four photos", func() HostApplicationRequest {
			r := validCommunity()
			p := testBucket + "general/p.png"
			r.PortfolioPhotoURLs = []string{p, p, p, p}
			return r
		}, "too_many_photos"},
		{"id outside the private folder", func() HostApplicationRequest {
			r := validCommunity()
			r.GovernmentIDURL = sp(testBucket + "general/id.png")
			return r
		}, "invalid_file_url"},
		{"selfie on another host", func() HostApplicationRequest {
			r := validCommunity()
			r.SelfieURL = sp("https://evil.example/hosts/government-ids/me.png")
			return r
		}, "invalid_file_url"},
		{"photo over http", func() HostApplicationRequest {
			r := validCommunity()
			r.PortfolioPhotoURLs = []string{"http://b.s3.ap-south-1.amazonaws.com/general/p.png"}
			return r
		}, "invalid_file_url"},
		{"community name too long", func() HostApplicationRequest {
			r := validCommunity()
			r.CommunityName = sp(strings.Repeat("a", 81))
			return r
		}, "field_too_long"},
		{"too many languages", func() HostApplicationRequest {
			r := validProfessional()
			r.Languages = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
			return r
		}, "field_too_long"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateHostApplication(c.mod(), testBucket)
			got := ""
			if err != nil {
				got = err.Code
				if err.Message == "" {
					t.Fatal("empty message")
				}
			}
			if got != c.want {
				t.Fatalf("got %q (%v), want %q", got, err, c.want)
			}
		})
	}
}

func TestValidateHostApplication_NoBucketChecksHTTPSOnly(t *testing.T) {
	r := validCommunity()
	r.GovernmentIDURL = sp("https://anything.example/id.png")
	if err := ValidateHostApplication(r, ""); err != nil {
		t.Fatalf("without a bucket configured only https is checked: %v", err)
	}
}
