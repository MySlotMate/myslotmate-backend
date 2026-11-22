package controller

import (
	"reflect"
	"strings"
	"testing"
)

// Private application documents must never be on the public host DTO
// (GET /hosts, GET /hosts/{slug}).
func TestPublicHostProfile_OmitsPrivateURLs(t *testing.T) {
	private := map[string]bool{"government_id_url": true, "selfie_url": true, "certificate_url": true, "degree_url": true}
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.Anonymous {
				walk(f.Type)
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if private[name] {
				t.Errorf("PublicHostProfile exposes private field %q", name)
			}
		}
	}
	walk(reflect.TypeOf(PublicHostProfile{}))
}
