package agent

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseBreakdownReadsFencedJSON(t *testing.T) {
	// The shape a model actually returns: a sentence, a fence, a language tag.
	content := "Sure! Here is the split:\n\n```json\n" + `{"subtasks": [
  {"title": "schema", "goal": "write the schema", "writes": ["schema.json"], "reads": []},
  {"title": "impl", "goal": "write the board", "writes": ["board.js"], "reads": ["schema.json"]}
]}` + "\n```\n"

	subs, err := parseBreakdown(content)
	if err != nil {
		t.Fatalf("parseBreakdown: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("got %d subtasks, want 2: %+v", len(subs), subs)
	}
	if subs[1].Title != "impl" || subs[1].Goal != "write the board" {
		t.Errorf("second subtask = %+v", subs[1])
	}
	if !reflect.DeepEqual(subs[1].Reads, []string{"schema.json"}) {
		t.Errorf("reads = %v", subs[1].Reads)
	}
}

// The breakdown object has to be found by its own key. Sharing the step
// parser's envelope keys would make it match a "tool" object in the commentary.
func TestParseBreakdownSkipsIncidentalObjects(t *testing.T) {
	content := `Here is an example of the format: {"goal_met": false, "next_action": "x"}

	{"subtasks": [
	  {"title": "a", "goal": "do a", "writes": ["a.md"]},
	  {"title": "b", "goal": "do b", "writes": ["b.md"]}
	]}`

	subs, err := parseBreakdown(content)
	if err != nil {
		t.Fatalf("parseBreakdown: %v", err)
	}
	if len(subs) != 2 || subs[0].Title != "a" {
		t.Errorf("got %+v", subs)
	}
}

func TestParseBreakdownRejectsNonBreakdowns(t *testing.T) {
	for name, content := range map[string]string{
		"prose":         "I would split this into a schema task and an implementation task.",
		"wrong object":  `{"tasks": [{"title": "a"}]}`,
		"broken syntax": `{"subtasks": [{"title": "a", ]}`,
	} {
		if _, err := parseBreakdown(content); err == nil {
			t.Errorf("%s: parsed as a breakdown, want an error", name)
		}
	}
}

// A claim is keyed on the path relative to the workspace root, so the parser has
// to reduce what the model typed to that same key. Otherwise "./board.js" and
// "board.js" pass validation as two paths and collide in the database, where the
// only remedy left is the fallback.
func TestParseBreakdownNormalizesPaths(t *testing.T) {
	content := `{"subtasks": [
	  {"title": "a", "goal": "g", "writes": ["./src/a.js", "/src/b.js", "src/./a.js"], "reads": ["  c.md  ", "", "../escape.md"]},
	  {"title": "b", "goal": "g", "writes": ["dir/../d.js"]}
	]}`

	subs, err := parseBreakdown(content)
	if err != nil {
		t.Fatalf("parseBreakdown: %v", err)
	}
	// The repeat collapses, the leading slash and dot go, and the path that
	// escapes the workspace is dropped rather than carried to a claim that
	// would refuse it.
	if got := subs[0].Writes; !reflect.DeepEqual(got, []string{"src/a.js", "src/b.js"}) {
		t.Errorf("writes = %v", got)
	}
	if got := subs[0].Reads; !reflect.DeepEqual(got, []string{"c.md"}) {
		t.Errorf("reads = %v", got)
	}
	if got := subs[1].Writes; !reflect.DeepEqual(got, []string{"d.js"}) {
		t.Errorf("writes = %v", got)
	}
}

func TestParseBreakdownTitlesFromGoal(t *testing.T) {
	content := `{"subtasks": [{"goal": "write the schema for the board", "writes": ["a.json"]},
	                          {"title": "b", "goal": "g", "writes": ["b.json"]}]}`
	subs, err := parseBreakdown(content)
	if err != nil {
		t.Fatalf("parseBreakdown: %v", err)
	}
	// A missing label is not worth rejecting a good partition over.
	if subs[0].Title != "write the schema for the board" {
		t.Errorf("title = %q, want it derived from the goal", subs[0].Title)
	}
}

// The failure the whole validation pass exists for.
func TestValidateRejectsTwoWritersOfOnePath(t *testing.T) {
	err := validateBreakdown([]Subtask{
		{Title: "impl", Goal: "g", Writes: []string{"board.js", "util.js"}},
		{Title: "tests", Goal: "g", Writes: []string{"board.js"}},
	})
	if err == nil {
		t.Fatal("a contested path passed validation")
	}
	// The message is fed back to the model verbatim, so it has to name the path
	// and both claimants or the retry has nothing to act on.
	for _, want := range []string{"board.js", "impl", "tests"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "util.js") {
		t.Errorf("error %q implicates an uncontested path", err)
	}
}

func TestValidateAcceptsACleanPartition(t *testing.T) {
	err := validateBreakdown([]Subtask{
		{Title: "schema", Goal: "g", Writes: []string{"schema.json"}},
		{Title: "impl", Goal: "g", Writes: []string{"board.js"}, Reads: []string{"schema.json"}},
		// The integration node: it owns the shared file and reads its siblings.
		{Title: "index", Goal: "g", Writes: []string{"index.html"}, Reads: []string{"board.js", "schema.json"}},
	})
	if err != nil {
		t.Errorf("a clean partition was rejected: %v", err)
	}
}

func TestValidateRejectsUnschedulablePlans(t *testing.T) {
	cases := map[string]struct {
		subs []Subtask
		want string
	}{
		"empty": {nil, "no subtasks"},
		"one subtask": {[]Subtask{
			{Title: "everything", Goal: "g", Writes: []string{"a.md"}},
		}, "single subtask"},
		"a subtask that writes nothing": {[]Subtask{
			{Title: "a", Goal: "g", Writes: []string{"a.md"}},
			{Title: "reader", Goal: "g", Reads: []string{"a.md"}},
		}, "reader"},
		"a subtask with no goal": {[]Subtask{
			{Title: "a", Goal: "g", Writes: []string{"a.md"}},
			{Title: "b", Writes: []string{"b.md"}},
		}, "no goal"},
	}
	for name, tc := range cases {
		err := validateBreakdown(tc.subs)
		if err == nil {
			t.Errorf("%s: passed validation, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, tc.want)
		}
	}
}

func TestValidateRejectsTooManySubtasks(t *testing.T) {
	subs := make([]Subtask, maxSubtasks+1)
	for i := range subs {
		subs[i] = Subtask{Title: "t", Goal: "g", Writes: []string{string(rune('a'+i)) + ".md"}}
	}
	if err := validateBreakdown(subs); err == nil {
		t.Error("a plan past the subtask cap passed validation")
	}
}

// A cycle is as fatal as a contested path and just as re-plannable, so it is
// caught here rather than in PlanGroup — the difference between spending a retry
// and falling back to one serial task.
func TestValidateRejectsCyclicReads(t *testing.T) {
	err := validateBreakdown([]Subtask{
		{Title: "a", Goal: "g", Writes: []string{"a.md"}, Reads: []string{"b.md"}},
		{Title: "b", Goal: "g", Writes: []string{"b.md"}, Reads: []string{"a.md"}},
		{Title: "c", Goal: "g", Writes: []string{"c.md"}},
	})
	if err == nil {
		t.Fatal("a read cycle passed validation")
	}
	for _, want := range []string{"a", "b"} {
		if !strings.Contains(err.Error(), `"`+want+`"`) {
			t.Errorf("error %q does not name the subtask %q in the cycle", err, want)
		}
	}
	if strings.Contains(err.Error(), `"c"`) {
		t.Errorf("error %q implicates a subtask outside the cycle", err)
	}
}

// Reading what you also write is not an edge, so it cannot be a cycle either.
func TestValidateAllowsSelfReads(t *testing.T) {
	err := validateBreakdown([]Subtask{
		{Title: "a", Goal: "g", Writes: []string{"a.md"}, Reads: []string{"a.md"}},
		{Title: "b", Goal: "g", Writes: []string{"b.md"}},
	})
	if err != nil {
		t.Errorf("self-read rejected: %v", err)
	}
}

func TestIdeaTitleClipsToOneLine(t *testing.T) {
	if got := ideaTitle("build a tetris clone\nwith sound"); got != "build a tetris clone" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 200)
	got := ideaTitle(long)
	if len([]rune(got)) != ideaTitleLen {
		t.Errorf("title is %d runes, want %d", len([]rune(got)), ideaTitleLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a clipped title should say so: %q", got)
	}
	if got := ideaTitle("   "); got != "Untitled idea" {
		t.Errorf("got %q", got)
	}
}
