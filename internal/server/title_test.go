package server

import "testing"

// A chain of continues and retries must not stack suffixes, and the counter it
// reads comes from a user-supplied title: anything that is not a plain positive
// integer has to fall through to a fresh " (2)" rather than be trusted.
func TestNextTitle(t *testing.T) {
	cases := map[string]string{
		"port a script":               "port a script (2)",
		"port a script (2)":           "port a script (3)",
		"port a script (9)":           "port a script (10)",
		"port a script  ":             "port a script (2)",
		"notes (draft)":               "notes (draft) (2)",
		"budget (0)":                  "budget (0) (2)",
		"budget (-1)":                 "budget (-1) (2)",
		"spacing ( 2)":                "spacing ( 2) (2)",
		"(2)":                         "(2) (2)",
		"nested (a (2)) (3)":          "nested (a (2)) (4)",
		"unclosed (2":                 "unclosed (2 (2)",
		"huge (99999999999999999999)": "huge (99999999999999999999) (2)",
	}
	for title, want := range cases {
		if got := nextTitle(title); got != want {
			t.Errorf("nextTitle(%q) = %q, want %q", title, got, want)
		}
	}
}
