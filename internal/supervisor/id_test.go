package supervisor

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateID(t *testing.T) {
	cases := []struct {
		name string
		id   string
		ok   bool
	}{
		// valid
		{"simple", "myapp", true},
		{"with hyphen", "test-app", true},
		{"with digits", "pr-123", true},
		{"single char", "a", true},
		{"starts with digit", "0app", true},
		{"max length", strings.Repeat("a", 63), true},

		// invalid: shape
		{"empty", "", false},
		{"too long", strings.Repeat("a", 64), false},
		{"leading hyphen", "-foo", false},

		// invalid: characters — these are the security-relevant ones
		// because the ID becomes a directory name, cgroup slice
		// element, netns name, and state.json key.
		{"slash (path sep)", "foo/bar", false},
		{"backslash", "foo\\bar", false},
		{"dot dot dot (traversal)", "..", false},
		{"hidden traversal", "foo/../bar", false},
		{"null byte", "foo\x00bar", false},
		{"newline", "foo\nbar", false},
		{"space", "foo bar", false},
		{"uppercase", "FooBar", false},
		{"underscore", "foo_bar", false},
		{"dot", "foo.bar", false},
		{"shell metachar", "$(rm -rf /)", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateID(c.id)
			if c.ok && err != nil {
				t.Errorf("ValidateID(%q) = %v, want nil", c.id, err)
			}
			if !c.ok {
				if err == nil {
					t.Errorf("ValidateID(%q) = nil, want error", c.id)
				}
				if !errors.Is(err, ErrInvalidID) {
					t.Errorf("ValidateID(%q): err = %v, want errors.Is ErrInvalidID", c.id, err)
				}
			}
		})
	}
}
