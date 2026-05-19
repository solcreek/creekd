package supervisor

import (
	"errors"
	"fmt"
)

// ErrInvalidID is returned when an app ID does not match the
// supervisor's accepted grammar.
var ErrInvalidID = errors.New("supervisor: invalid app id")

// MaxIDLen is the maximum number of bytes in an app ID. Bounded so
// the derived names (cgroup slice, netns, log dir, state key) stay
// well under any kernel path-element limits and remain ergonomic in
// CLI output.
const MaxIDLen = 63

// ValidateID enforces the app-ID grammar:
//
//	^[a-z0-9][a-z0-9-]{0,62}$
//
// Lowercase ASCII, digits, and internal hyphens. Must not start with
// a hyphen. Length 1..63. No slashes, no dots, no whitespace, no
// uppercase, no unicode.
//
// The ID becomes a directory name (logs), a cgroup slice element,
// a netns name, and a key in state.json. Restricting to this
// character set means none of those derived names can contain path
// separators, parent-dir traversal, shell metacharacters, or
// case-insensitive collisions on filesystems that fold case.
func ValidateID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty", ErrInvalidID)
	}
	if len(id) > MaxIDLen {
		return fmt.Errorf("%w: %d > %d bytes", ErrInvalidID, len(id), MaxIDLen)
	}
	for i, b := range []byte(id) {
		switch {
		case b >= 'a' && b <= 'z':
			continue
		case b >= '0' && b <= '9':
			continue
		case b == '-':
			if i == 0 {
				return fmt.Errorf("%w: must not start with hyphen", ErrInvalidID)
			}
			continue
		default:
			return fmt.Errorf("%w: byte %d (%q) not in [a-z0-9-]",
				ErrInvalidID, i, string(b))
		}
	}
	return nil
}
