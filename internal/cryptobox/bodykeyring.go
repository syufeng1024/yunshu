// Body-key ring: request-body KEK rotation with backwards read.
//
// Request bodies are sealed with their own KEK (LOG_BODY_KEK), never with
// CHANNEL_KEY_KEK, and both ciphertext families use different AADs. That
// separation is unchanged here; what changes is that the body key now has a
// **versioned** identity stored next to every row (t_request_body.body_key_id)
// so a rotation does not orphan the rows written before it.
//
// Before this, body-v1 was a compile-time constant and the reader accepted
// exactly that id: rotating LOG_BODY_KEK made every stored body unreadable and
// turned the body endpoint into a 404 for the whole 180-day retention window
// (G3, errata E-39).
package cryptobox

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Body key environment variables.
//
//	LOG_BODY_KEK            base64 of the CURRENT 32-byte body KEK (unchanged;
//	                        its name is owned by internal/config, which is why
//	                        BuildBodyKeyRing takes the value as an argument)
//	LOG_BODY_KEY_ID         id written to new rows; empty means DefaultBodyKeyID
//	LOG_BODY_KEK_PREVIOUS   retired keys, "id:base64,id:base64"
//
// The previous list is what makes a rotation read-only-safe: the current key
// seals new rows, the retired ones keep opening the old rows until they age out
// of the retention window.
const (
	EnvBodyKeyID       = "LOG_BODY_KEY_ID"
	EnvBodyKeyPrevious = "LOG_BODY_KEK_PREVIOUS"
)

// DefaultBodyKeyID is the id used when LOG_BODY_KEY_ID is unset. It is the
// value every row written before this change already carries, so a deployment
// that does not configure rotation keeps working byte-for-byte.
const DefaultBodyKeyID = BodyKeyID

// MaxBodyKeyIDLen matches t_request_body.body_key_id (varchar(32)). An id
// longer than this would be truncated by MySQL and the row would name a key
// nobody can look up.
const MaxBodyKeyIDLen = 32

// Ring errors are value-free: they never echo a KEK or an envelope. They do
// name the offending id, because an operator needs to know which entry to fix.
var (
	ErrBadBodyKeyID       = fmt.Errorf("cryptobox: body key id must be 1..%d chars, no ':' or ','", MaxBodyKeyIDLen)
	ErrDuplicateBodyKeyID = fmt.Errorf("cryptobox: duplicate body key id")
	ErrBadPreviousEntry   = fmt.Errorf("cryptobox: %s must be id:base64 pairs separated by ','", EnvBodyKeyPrevious)
	ErrNoCurrentBodyKey   = fmt.Errorf("cryptobox: the current body key is not configured")
	ErrUnknownBodyKeyID   = fmt.Errorf("cryptobox: no key configured for this body_key_id")
)

// BodyKeyRing holds the current body key plus the retired ones, all built with
// the request-body AAD. It is immutable after construction and safe for
// concurrent use (cipher.AEAD is).
type BodyKeyRing struct {
	current string
	boxes   map[string]*Box
}

// NewBodyKeyRing parses the three environment values into a ring.
//
// currentKEK is base64; previous is the raw LOG_BODY_KEK_PREVIOUS value
// ("id:base64,id:base64"). Empty previous means "no retired keys", which is the
// pre-rotation state.
//
// Every problem is an error rather than a silent skip: a body key that silently
// disappears makes stored bodies unreadable, and the operator would only find
// out through a support ticket. The daemons log this error and degrade to "no
// body storage" — the data plane keeps serving (same policy as a missing
// LOG_BODY_KEK), but the reason is always printed.
func NewBodyKeyRing(currentID, currentKEK, previous string) (*BodyKeyRing, error) {
	id, err := normalizeBodyKeyID(currentID)
	if err != nil {
		return nil, err
	}
	currentBox, err := NewBodyFromBase64(currentKEK)
	if err != nil {
		// ErrNoBodyKEK / ErrInvalidKEK, already value-free.
		return nil, err
	}
	boxes := map[string]*Box{id: currentBox}

	retired, err := ParsePreviousBodyKeys(previous)
	if err != nil {
		return nil, err
	}
	for _, entry := range retired {
		if _, exists := boxes[entry.ID]; exists {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateBodyKeyID, entry.ID)
		}
		box, berr := NewBodyFromBase64(entry.KEK)
		if berr != nil {
			return nil, fmt.Errorf("%w (id=%s)", berr, entry.ID)
		}
		boxes[entry.ID] = box
	}
	return &BodyKeyRing{current: id, boxes: boxes}, nil
}

// NewBodyKeyRingFromBoxes builds a ring from already-constructed boxes. It is
// the seam tests and single-key deployments use; it enforces the same id rules
// and the same duplicate check as NewBodyKeyRing.
func NewBodyKeyRingFromBoxes(currentID string, boxes map[string]*Box) (*BodyKeyRing, error) {
	id, err := normalizeBodyKeyID(currentID)
	if err != nil {
		return nil, err
	}
	if len(boxes) == 0 {
		return nil, ErrNoCurrentBodyKey
	}
	out := make(map[string]*Box, len(boxes))
	for keyID, box := range boxes {
		norm, nerr := normalizeBodyKeyID(keyID)
		if nerr != nil {
			return nil, nerr
		}
		if _, dup := out[norm]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateBodyKeyID, norm)
		}
		if box == nil {
			return nil, fmt.Errorf("%w (id=%s)", ErrNoCurrentBodyKey, norm)
		}
		out[norm] = box
	}
	if _, ok := out[id]; !ok {
		return nil, fmt.Errorf("%w (id=%s)", ErrNoCurrentBodyKey, id)
	}
	return &BodyKeyRing{current: id, boxes: out}, nil
}

// BodyKeySpec is one retired key as parsed from LOG_BODY_KEK_PREVIOUS.
type BodyKeySpec struct {
	ID  string
	KEK string
}

// ParsePreviousBodyKeys splits "id:base64,id:base64". Empty segments (a
// trailing comma, or an empty variable) are ignored so a hand-edited value does
// not fail on a stray separator; anything else must be a complete pair.
func ParsePreviousBodyKeys(previous string) ([]BodyKeySpec, error) {
	previous = strings.TrimSpace(previous)
	if previous == "" {
		return nil, nil
	}
	var out []BodyKeySpec
	for _, raw := range strings.Split(previous, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		id, kek, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("%w: %q has no ':'", ErrBadPreviousEntry, maskEntry(entry))
		}
		id, err := normalizeBodyKeyID(id)
		if err != nil {
			return nil, err
		}
		kek = strings.TrimSpace(kek)
		if kek == "" {
			return nil, fmt.Errorf("%w: %q has an empty key", ErrBadPreviousEntry, id)
		}
		out = append(out, BodyKeySpec{ID: id, KEK: kek})
	}
	return out, nil
}

// BuildBodyKeyRing reads LOG_BODY_KEY_ID and LOG_BODY_KEK_PREVIOUS and builds
// the ring around currentKEK (the value of LOG_BODY_KEK).
//
// currentKEK is an argument rather than an env read so this package does not
// need internal/config just for a variable name; every caller already has the
// value in hand. An unset LOG_BODY_KEY_ID means DefaultBodyKeyID, so a
// deployment that never rotates behaves exactly as before.
func BuildBodyKeyRing(currentKEK string) (*BodyKeyRing, error) {
	previous := os.Getenv(EnvBodyKeyPrevious)
	keyID := strings.TrimSpace(os.Getenv(EnvBodyKeyID))
	if keyID == "" {
		keyID = DefaultBodyKeyID
	}
	return NewBodyKeyRing(keyID, currentKEK, previous)
}

// CurrentID is the id written to new rows.
func (r *BodyKeyRing) CurrentID() string {
	if r == nil {
		return ""
	}
	return r.current
}

// KeyIDs lists every configured id, sorted. It is what a startup log prints so
// an operator can see which old rows this node can still open.
func (r *BodyKeyRing) KeyIDs() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.boxes))
	for id := range r.boxes {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Has reports whether the ring can open a row carrying keyID.
func (r *BodyKeyRing) Has(keyID string) bool {
	if r == nil {
		return false
	}
	_, ok := r.boxes[normalizeQuiet(keyID)]
	return ok
}

// Seal encrypts plaintext with the current key and returns both the envelope
// and the id the caller must store next to it.
func (r *BodyKeyRing) Seal(plaintext []byte) (string, string, error) {
	if r == nil {
		return "", "", ErrNoCurrentBodyKey
	}
	box, ok := r.boxes[r.current]
	if !ok {
		return "", "", ErrNoCurrentBodyKey
	}
	envelope, err := box.Seal(plaintext)
	if err != nil {
		return "", "", err
	}
	return r.current, envelope, nil
}

// Open decrypts an envelope with the key named by the row. An id the ring does
// not know is ErrUnknownBodyKeyID — never a fallback to the current key, which
// would report "authentication failed" for a row whose only problem is that its
// key was retired too early.
func (r *BodyKeyRing) Open(keyID, envelope string) ([]byte, error) {
	if r == nil {
		return nil, ErrNoCurrentBodyKey
	}
	box, ok := r.boxes[normalizeQuiet(keyID)]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownBodyKeyID, normalizeQuiet(keyID))
	}
	return box.Open(envelope)
}

// normalizeBodyKeyID trims and validates an id.
func normalizeBodyKeyID(id string) (string, error) {
	id = normalizeQuiet(id)
	if id == "" || len(id) > MaxBodyKeyIDLen || strings.ContainsAny(id, ":,") {
		return "", ErrBadBodyKeyID
	}
	return id, nil
}

func normalizeQuiet(id string) string { return strings.TrimSpace(id) }

// maskEntry keeps a malformed LOG_BODY_KEK_PREVIOUS entry out of the error
// text: it may contain key material. Only the id part, truncated, survives.
func maskEntry(entry string) string {
	if i := strings.IndexByte(entry, ':'); i >= 0 {
		entry = entry[:i]
	}
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "<empty>"
	}
	if len(entry) > MaxBodyKeyIDLen {
		entry = entry[:MaxBodyKeyIDLen]
	}
	return entry
}
