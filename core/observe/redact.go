package observe

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// Mode decides how much of a value — a query filter, an update, a request
// body — is written onto a span.
//
// The shape is always recorded: field names, operators, how many elements an
// `in` clause had. Those are the schema, not the data, and they are most of
// what makes a trace answer "which query did this, and how much did it touch".
// What differs is the values.
type Mode int

const (
	// Safe records a value only when it cannot be personal data: numbers,
	// booleans, null, and strings that are identifiers — a UUID, a ULID, a
	// hex digest, a number written as a string. Everything else becomes "?".
	//
	// It is the zero value, and therefore the default everywhere, because it
	// keeps the id — which is how you find the document again — while dropping
	// the name, the address and the search term. Nothing is rendered at all
	// unless a tracer is installed; see Enabled.
	Safe Mode = iota
	// Full records every value except the ones that can never be recorded:
	// anything under a sensitive field name, and anything that looks like an
	// email address wherever it appears. For a development machine, or a
	// service holding no personal data at all.
	Full
	// Off records nothing — not the values, not the field names. For a service
	// whose schema is itself sensitive, or a deployment that will not have
	// this argument.
	Off
)

// Limits on what one value may cost. A filter with ten thousand ids in it is
// a real thing, and a span attribute is not the place for all of them.
const (
	maxRendered = 1024 // bytes of rendered output, then it is cut
	maxElements = 8    // elements of an array before the rest becomes a count
	maxDepth    = 6
)

// redacted is what stands in for a value that was not recorded. Short, and
// not something a value could be mistaken for.
const redacted = "?"

// Render writes v the way a span should carry it: keys sorted so two identical
// queries produce identical text, values filtered by mode, and the whole thing
// bounded. It is JSON-shaped rather than JSON — a truncated document would not
// parse, and pretending otherwise invites something downstream to try.
//
//	db.M{"org_id": db.M{"$eq": "0193..."}, "email": db.M{"$eq": "a@b.c"}}
//	-> {"email":{"$eq":"?"},"org_id":{"$eq":"0193..."}}
//
// Render is only worth calling when something is listening; see Enabled.
func Render(v any, mode Mode) string {
	if mode == Off {
		return ""
	}
	var b strings.Builder
	write(&b, v, mode, false, 0)
	out := b.String()
	if len(out) > maxRendered {
		// Cut on a rune boundary: a half-written multi-byte character in an
		// attribute is a mojibake bug reported against the tracing backend.
		cut := maxRendered
		for cut > 0 && !isBoundary(out[cut]) {
			cut--
		}
		return out[:cut] + "…"
	}
	return out
}

func isBoundary(b byte) bool { return b&0xC0 != 0x80 }

// Sensitive reports whether a field name names something that must not be
// recorded whatever the mode: a credential, a contact detail, a government
// number. Matched by word, not by substring, so "api_key" and "key" are
// sensitive and "keyword" and "monkey" are not.
//
// Words are separators, underscores and case changes alike: a JSON document is
// as likely to say "userEmail" as "user_email", and splitting only on
// punctuation made the first one ordinary. That was not a theoretical gap —
// "phoneNumber" holding digits is a string Safe would otherwise have kept,
// because digits are how an id looks.
func Sensitive(field string) bool {
	for _, word := range words(field) {
		if sensitiveWords[word] {
			return true
		}
	}
	return false
}

// words splits a field name into lower-case words on punctuation and on case
// boundaries: user_email, userEmail and UserEmail all give [user email], and
// APIKey gives [api key] rather than [apikey].
func words(field string) []string {
	var out []string
	var word []rune
	flush := func() {
		if len(word) > 0 {
			out = append(out, strings.ToLower(string(word)))
			word = word[:0]
		}
	}
	runes := []rune(field)
	for i, r := range runes {
		switch {
		case !isAlnum(r):
			flush()
			continue
		case isUpper(r) && i > 0 && !isUpper(runes[i-1]):
			flush() // userEmail, user2Email
		case isUpper(r) && i+1 < len(runes) && !isUpper(runes[i+1]) && !isDigit(runes[i+1]):
			flush() // APIKey: the K starts a word because the e after it is lower
		}
		word = append(word, r)
	}
	flush()
	return out
}

func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || isDigit(r)
}
func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// The list errs towards recording less. Adding to it is cheap; discovering
// that a year of traces holds everyone's phone number is not.
var sensitiveWords = map[string]bool{
	"email": true, "emails": true, "mail": true,
	"password": true, "passwd": true, "pwd": true, "pass": true,
	"secret": true, "token": true, "key": true, "apikey": true, "auth": true,
	"authorization": true, "credential": true, "credentials": true,
	"cookie": true, "session": true, "otp": true, "pin": true,
	"phone": true, "tel": true, "telephone": true, "mobile": true, "msisdn": true,
	"address": true, "street": true, "postcode": true, "zip": true, "zipcode": true,
	"ssn": true, "nino": true, "tfn": true, "passport": true, "licence": true, "license": true,
	"dob": true, "birth": true, "birthdate": true, "birthday": true, "born": true,
	"card": true, "pan": true, "cvv": true, "cvc": true, "iban": true, "bsb": true,
	"latitude": true, "longitude": true, "lat": true, "lng": true,
}

// personal is the value-level rule, applied wherever a string appears and in
// every mode. An email address is the one piece of personal data that is
// recognisable without knowing the schema — and it is the one that turns a
// trace into a mailing list.
func personal(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	return strings.IndexByte(s[at+1:], '.') > 0
}

// identifier reports whether a string is a value you can safely keep: it names
// a row rather than describing a person. UUID (howl's own ids are UUIDv7),
// ULID, a hex digest, an ObjectID, or a plain integer.
func identifier(s string) bool {
	switch {
	case len(s) == 0 || len(s) > 64:
		return false
	case isUUID(s):
		return true
	case len(s) == 26 && isAll(s, base32Crockford):
		return true // ULID
	case (len(s) == 24 || len(s) == 32 || len(s) == 40 || len(s) == 64) && isAll(s, isHex):
		return true
	case allDigits(s):
		return true
	case timestamp(s):
		return true
	default:
		return false
	}
}

// timestamp reports whether s is an RFC 3339 date-time: a moment, which names
// no one. Keeping it is what makes a range query legible — the difference
// between knowing that a bulk delete scanned by date and knowing what it
// deleted, which is the question being asked at the moment somebody reads the
// span.
//
// A date-time, not a date. "1985-03-12" is how a date of birth is written and
// "2026-01-02T03:04:05Z" is not how anybody writes one by hand; a DOB stored
// as a time.Time does marshal to the second form, which is why dob, birth,
// birthdate, birthday and born are in sensitiveWords in every spelling. The
// shape is checked rather than the ranges: a string shaped exactly like a
// timestamp but reading 2026-13-99 is not somebody's personal data either.
func timestamp(s string) bool {
	if len(s) < 20 || len(s) > 35 {
		return false
	}
	if !allDigits(s[0:4]) || s[4] != '-' || !allDigits(s[5:7]) || s[7] != '-' || !allDigits(s[8:10]) {
		return false
	}
	if s[10] != 'T' && s[10] != 't' {
		return false
	}
	if !allDigits(s[11:13]) || s[13] != ':' || !allDigits(s[14:16]) || s[16] != ':' || !allDigits(s[17:19]) {
		return false
	}
	rest := s[19:]
	if len(rest) > 1 && rest[0] == '.' {
		i := 1
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 1 {
			return false // a decimal point with nothing after it
		}
		rest = rest[i:]
	}
	switch {
	case rest == "Z" || rest == "z":
		return true
	case len(rest) == 6 && (rest[0] == '+' || rest[0] == '-') &&
		allDigits(rest[1:3]) && rest[3] == ':' && allDigits(rest[4:6]):
		return true
	default:
		return false
	}
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	return isAll(s, func(b byte) bool { return b >= '0' && b <= '9' })
}

func isUUID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !isHex(s[i]) {
			return false
		}
	}
	return true
}

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func base32Crockford(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func isAll(s string, ok func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !ok(s[i]) {
			return false
		}
	}
	return true
}

// write renders v. hidden is true once a sensitive field name has been seen:
// the whole subtree under "credentials" is out, not only its leaves.
func write(b *strings.Builder, v any, mode Mode, hidden bool, depth int) {
	if depth > maxDepth {
		b.WriteString(`"…"`)
		return
	}
	if b.Len() > maxRendered {
		return // the caller truncates; stop building what it will cut
	}
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case string:
		writeStr(b, t, mode, hidden)
	case json.RawMessage:
		var decoded any
		if json.Unmarshal(t, &decoded) != nil {
			b.WriteString(`"?"`)
			return
		}
		write(b, decoded, mode, hidden, depth)
	case []byte:
		write(b, json.RawMessage(t), mode, hidden, depth)
	case map[string]any:
		writeMap(b, t, mode, hidden, depth)
	case []any:
		writeSlice(b, t, mode, hidden, depth)
	default:
		writeOther(b, v, mode, hidden, depth)
	}
}

// writeOther covers the numbers and everything with a JSON encoding of its
// own: a db.M is a named map type, a Set is another, and a struct is what an
// endpoint's body actually is. One marshal, then the same rules as anything
// else — which is what makes a typed body and a raw filter render alike.
func writeOther(b *strings.Builder, v any, mode Mode, hidden bool, depth int) {
	switch t := v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		raw, _ := json.Marshal(t)
		b.Write(raw)
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		b.WriteString(`"?"`)
		return
	}
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		b.WriteString(`"?"`)
		return
	}
	// A string or number that marshalled to itself would recurse forever
	// through this branch; everything that reaches here as one of those has
	// already been handled above, so decoded is a map, a slice or a scalar.
	switch decoded.(type) {
	case map[string]any, []any:
		write(b, decoded, mode, hidden, depth)
	default:
		write(b, decoded, mode, hidden, depth+1)
	}
}

func writeMap(b *strings.Builder, m map[string]any, mode Mode, hidden bool, depth int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // two identical queries have to render identically
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		if b.Len() > maxRendered {
			b.WriteString(`"…"`)
			break
		}
		// A key is a field name or an operator, both of which are schema —
		// except when the data put a value there, which an email test catches.
		key := k
		if personal(k) {
			key = redacted
		}
		quote(b, key)
		b.WriteByte(':')
		write(b, m[k], mode, hidden || Sensitive(k), depth+1)
	}
	b.WriteByte('}')
}

func writeSlice(b *strings.Builder, list []any, mode Mode, hidden bool, depth int) {
	b.WriteByte('[')
	for i, v := range list {
		if i == maxElements {
			// The count is the point: an `in` over five ids and one over five
			// thousand are different queries with the same shape.
			b.WriteString(`"+` + strconv.Itoa(len(list)-maxElements) + ` more"`)
			break
		}
		if i > 0 {
			b.WriteByte(',')
		}
		write(b, v, mode, hidden, depth+1)
	}
	b.WriteByte(']')
}

func writeStr(b *strings.Builder, s string, mode Mode, hidden bool) {
	if hidden || personal(s) || (mode != Full && !identifier(s)) {
		b.WriteString(`"` + redacted + `"`)
		return
	}
	quote(b, s)
}

// quote writes s as a JSON string, without the marshal-and-copy that doing it
// through encoding/json costs. Field names and identifiers are overwhelmingly
// plain ASCII, and that path writes straight into the builder; anything with a
// quote, a backslash, a control character or a multi-byte rune in it falls
// back. Worth the twenty lines: it is two allocations per value on a filter
// that may have dozens, on every traced operation.
func quote(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' || c >= 0x80 {
			raw, err := json.Marshal(s)
			if err != nil {
				b.WriteString(`"` + redacted + `"`)
				return
			}
			b.Write(raw)
			return
		}
	}
	b.WriteByte('"')
	b.WriteString(s)
	b.WriteByte('"')
}

// Enabled reports whether anything is listening. Rendering a filter costs an
// allocation and a walk of the query; on the no-op tracer that is a walk whose
// result is discarded, on every operation, forever.
func Enabled(ctx context.Context) bool {
	_, off := tracer(ctx).(Noop)
	return !off
}

// Text is Render for a value that is already one string — an id, a path
// parameter — and gives back the string itself rather than a quoted one, so a
// span attribute reads as the value and not as a fragment of JSON.
func Text(s string, mode Mode) string {
	switch {
	case mode == Off:
		return ""
	case personal(s):
		return redacted
	case mode == Full || identifier(s):
		return s
	default:
		return redacted
	}
}
