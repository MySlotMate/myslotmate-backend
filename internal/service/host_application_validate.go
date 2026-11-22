package service

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"myslotmate-backend/internal/models"
)

// ApplicationError is a submit-time rule failure from the two-path host
// application (frontend spec docs/backend-spec-host-application.md §3). Code
// is stable for clients; Message is a plain sentence the form shows as is.
type ApplicationError struct {
	Code    string
	Message string
}

func (e *ApplicationError) Error() string { return e.Code + ": " + e.Message }

var (
	moodKeys       = setOf("social", "adventurous", "wellness", "creative", "educational", "culinary", "cultural", "relaxing")
	professionKeys = setOf("fitness", "nutrition", "yoga", "therapy", "photography", "music",
		"business", "finance", "legal", "education", "career", "technology")
)

func setOf(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func appErr(code, msg string) *ApplicationError { return &ApplicationError{Code: code, Message: msg} }

func blank(s *string) bool { return s == nil || strings.TrimSpace(*s) == "" }

// privateDocsFolder is where ID, selfie, certificate and degree uploads live.
const privateDocsFolder = "hosts/government-ids/"

// ValidateHostApplication checks a submitted (not draft) application.
// bucketURL is the upload bucket's public URL prefix ("https://<bucket>.s3.<region>.amazonaws.com/");
// empty means S3 isn't configured and only https is checked.
//
// A body without host_type comes from an app build older than the two-path
// form and is not checked, so those builds can still apply.
// ponytail: drop that exemption once old builds are gone.
func ValidateHostApplication(r HostApplicationRequest, bucketURL string) *ApplicationError {
	switch r.HostType {
	case "":
		return nil
	case models.HostTypeCommunity, models.HostTypeProfessional:
	default:
		return appErr("invalid_host_type", "Choose community host or verified professional.")
	}

	if blank(r.GovernmentIDURL) {
		return appErr("government_id_required", "Please upload a government ID.")
	}

	if r.HostType == models.HostTypeCommunity {
		if r.CommunityCategory == nil || !moodKeys[*r.CommunityCategory] {
			return appErr("category_required", "Choose the kind of events you host.")
		}
		if r.Description == nil || utf8.RuneCountInString(strings.TrimSpace(*r.Description)) < 20 {
			return appErr("description_too_short", "Tell us a little more about your community.")
		}
	} else {
		if r.Profession == nil || !professionKeys[*r.Profession] {
			return appErr("profession_required", "Choose your field.")
		}
		if blank(r.YearsExperience) || blank(r.Specialization) {
			return appErr("experience_required", "Add your experience and specialization.")
		}
		if blank(r.CertificateURL) && blank(r.LicenseNumber) && blank(r.PortfolioURL) && blank(r.SocialLinkedin) {
			return appErr("credential_required", "Add a certificate, license number, portfolio or LinkedIn.")
		}
	}

	if len(r.PortfolioPhotoURLs) > 3 {
		return appErr("too_many_photos", "You can add up to 3 photos.")
	}

	badFile := appErr("invalid_file_url", "One of your files didn't upload properly. Please try again.")
	for _, u := range []*string{r.GovernmentIDURL, r.SelfieURL, r.CertificateURL, r.DegreeURL} {
		if !blank(u) && !uploadedTo(*u, bucketURL, privateDocsFolder) {
			return badFile
		}
	}
	for _, u := range append([]string{deref(r.TestimonialURL)}, r.PortfolioPhotoURLs...) {
		if u != "" && !uploadedTo(u, bucketURL, "") {
			return badFile
		}
	}

	for _, f := range []struct {
		name string
		v    *string
		max  int
	}{
		{"Community name", r.CommunityName, 80},
		{"License number", r.LicenseNumber, 60},
		{"Professional body", r.ProfessionalBody, 120},
		{"Specialization", r.Specialization, 120},
		{"Work history", r.WorkHistory, 500},
		{"Achievements", r.Achievements, 200},
	} {
		if f.v != nil && utf8.RuneCountInString(*f.v) > f.max {
			return appErr("field_too_long", fmt.Sprintf("%s can be at most %d characters.", f.name, f.max))
		}
	}
	if len(r.Languages) > 8 {
		return appErr("field_too_long", "You can list up to 8 languages.")
	}
	for _, l := range r.Languages {
		if strings.TrimSpace(l) == "" || utf8.RuneCountInString(l) > 30 {
			return appErr("field_too_long", "Each language can be at most 30 characters.")
		}
	}
	return nil
}

// uploadedTo reports whether u is an https URL in our bucket under folder.
// Without a bucket configured only the scheme is checked.
func uploadedTo(u, bucketURL, folder string) bool {
	if !strings.HasPrefix(u, "https://") {
		return false
	}
	if bucketURL == "" {
		return true
	}
	key, ok := strings.CutPrefix(u, bucketURL)
	return ok && strings.HasPrefix(key, folder) && !strings.Contains(key, "..")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
