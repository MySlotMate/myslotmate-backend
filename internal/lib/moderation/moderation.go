// Package moderation is the server-side copy of the web app's text rules
// (MySlotmate-Frontend src/lib/moderation.ts, pattern pass only). MySlotMate is
// a platonic companion platform: user text must not steer towards dating, move
// people off-platform (phone, email, UPI, DMs) or take payment outside it.
package moderation

import (
	"fmt"
	"regexp"
	"strings"
)

// Hard rules: any single hit rejects. These are the frontend's dating,
// contact-sharing and payment lists minus words too generic to reject alone
// ("single", "snap", "recharge", "long term"...), which would block ordinary
// captions like "single-origin coffee" or "a recharge weekend".
var hardLists = map[string][]string{
	"dating": {
		"looking for partner", "looking for girl", "looking for guy", "hangout date",
		"casual date", "serious relationship", "fwb", "friends with benefits",
		"soulmate", "vibe match",
	},
	"contact details": {
		"whatsapp me", "ping me", "dm me", "telegram", "signal app", "insta id",
		"instagram @", "snapchat", "contact me", "text me", "call me", "share number",
		"drop your number", "email me", "personal email", "connect privately",
	},
	"payment outside MySlotMate": {
		"advance payment", "send advance", "pay directly", "pay outside", "cash only",
		"no platform fee", "discount if direct", "transfer money", "wire transfer",
		"crypto payment", "bitcoin", "usdt", "wallet transfer",
	},
}

// Scored rules, ported with the frontend's weights: blocked when the 1–10
// score is above 3, so one stray ambiguous word ("hard climb") passes.
var scoredLists = map[string]struct {
	weight float64
	words  []string
}{
	"sexual": {5, []string{
		"hookup", "one night stand", "make out", "intimacy", "physical connection",
		"adult fun", "no boundaries", "private fun", "casual sex", "casual hookup",
		"no strings attached", "nsa", "sugar daddy", "sugar mama", "sugar baby",
		"escort", "call girl", "gigolo", "nsfw", "x-rated", "adult content", "erotic",
		"sensual", "passionate night", "steamy", "flirty", "seduction", "seduce",
		"horny", "aroused", "blowjob", "blow job", "handjob", "threesome", "orgy",
		"booty call", "quickie", "kinky", "bondage", "bdsm", "sexting", "nudes",
		"naked photos", "cam show", "stripper", "happy ending", "sexual favor",
		"sexual service", "paid sex", "prostitute", "paid companion", "wet", "hard",
		"cuddle",
	}},
	"profanity": {5, []string{
		"fuck", "shit", "asshole", "bastard", "bitch", "bullshit", "dickhead",
		"motherfucker", "motherfucking", "fucked", "fcuk",
	}},
	"unsafe meetup": {2.5, []string{
		"come alone", "private meetup", "secret location", "hidden place",
		"no public place", "my flat", "my room", "my apartment", "hotel room",
		"late night meetup", "no one will know",
	}},
	"dating": {1.5, []string{"single", "lonely", "need company", "chemistry", "long term", "short term"}},
	"manipulation": {1, []string{
		"trust me", "safe with me", "no questions asked", "keep it secret", "no rules",
		"don't tell anyone", "be discreet",
	}},
}

var (
	// Indian mobile numbers, with optional +91/91 and spaces or dashes between digits.
	phoneRe = regexp.MustCompile(`(?:^|\D)(?:\+?91[\s-]?)?[6-9](?:[\s-]?\d){9}(?:\D|$)`)
	// Email addresses and UPI handles (name@okaxis) share one shape.
	handleRe = regexp.MustCompile(`[\w.\-]+@[A-Za-z][\w\-]*`)

	hardRes   = compile(hardLists)
	scoredRes = func() map[string]*regexp.Regexp {
		m := map[string][]string{}
		for k, v := range scoredLists {
			m[k] = v.words
		}
		return compile(m)
	}()
)

func compile(lists map[string][]string) map[string]*regexp.Regexp {
	out := make(map[string]*regexp.Regexp, len(lists))
	for cat, words := range lists {
		quoted := make([]string, len(words))
		for i, w := range words {
			quoted[i] = regexp.QuoteMeta(w)
		}
		out[cat] = regexp.MustCompile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)\b`)
	}
	return out
}

// Check returns nil when text is acceptable, otherwise an error whose message
// is safe to show the user. field names the input ("Album title").
func Check(field, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if phoneRe.MatchString(text) || handleRe.MatchString(text) {
		return fmt.Errorf("%s can't include phone numbers, emails or UPI ids", field)
	}
	for cat, re := range hardRes {
		if re.MatchString(text) {
			return fmt.Errorf("%s can't include %s", field, cat)
		}
	}
	total := 0.0
	for cat, re := range scoredRes {
		total += float64(len(re.FindAllStringIndex(text, -1))) * scoredLists[cat].weight
	}
	// Frontend: score = clamp(ceil(total/2), 1, 10); blocked when score > 3.
	if total > 6 {
		return fmt.Errorf("%s doesn't meet our community guidelines", field)
	}
	return nil
}
