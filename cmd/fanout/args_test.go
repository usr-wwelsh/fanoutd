package main

import (
	"flag"
	"reflect"
	"testing"
)

func permuteFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Bool("full", false, "")
	fs.Int("last", 0, "")
	fs.String("goal", "", "")
	return fs
}

func TestPermute(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"already ordered", []string{"--full", "c762"}, []string{"--full", "c762"}},
		{"flag after positional", []string{"c762", "--full"}, []string{"--full", "c762"}},
		{"value flag after positional", []string{"c762", "--last", "3"}, []string{"--last", "3", "c762"}},
		{"equals form", []string{"c762", "--last=3"}, []string{"--last=3", "c762"}},
		{"single dash", []string{"c762", "-full"}, []string{"-full", "c762"}},
		{"mixed", []string{"c762", "--goal", "build it", "--full"}, []string{"--goal", "build it", "--full", "c762"}},
		{"multiple positionals", []string{"c762", "todo", "--full"}, []string{"--full", "c762", "todo"}},
		{"nothing", nil, nil},

		// A bool flag must not swallow the positional that follows it.
		{"bool then positional", []string{"--full", "c762", "--last", "2"}, []string{"--full", "--last", "2", "c762"}},

		// An unknown flag is left for Parse to reject rather than eating an
		// argument on a guess about its arity.
		{"unknown flag", []string{"c762", "--bogus", "x"}, []string{"--bogus", "c762", "x"}},

		// After --, everything is positional. This is what lets a title or
		// goal begin with a dash.
		{"double dash", []string{"--full", "--", "-weird-title", "--last"}, []string{"--full", "-weird-title", "--last"}},

		// A goal that starts with a dash is the reason -- exists.
		{"value that looks like a flag", []string{"c762", "--goal", "--not-a-flag"}, []string{"--goal", "--not-a-flag", "c762"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := permute(permuteFlags(), tt.in)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("permute(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The end that matters: after permuting, Parse sees the flags and the
// positional survives.
func TestPermuteThenParse(t *testing.T) {
	fs := permuteFlags()
	if err := fs.Parse(permute(fs, []string{"c762", "--last", "3", "--full"})); err != nil {
		t.Fatal(err)
	}
	if fs.Arg(0) != "c762" {
		t.Errorf("Arg(0) = %q", fs.Arg(0))
	}
	if fs.Lookup("last").Value.String() != "3" {
		t.Errorf("last = %q", fs.Lookup("last").Value)
	}
	if fs.Lookup("full").Value.String() != "true" {
		t.Errorf("full = %q", fs.Lookup("full").Value)
	}
}
