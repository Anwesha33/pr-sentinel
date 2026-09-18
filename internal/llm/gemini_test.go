package llm

import "testing"

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"bare object", `{"a":1}`, `{"a":1}`, true},
		{"fenced", "```json\n{\"a\":1}\n```", `{"a":1}`, true},
		{"fenced without language", "```\n{\"a\":1}\n```", `{"a":1}`, true},
		{"prose around it", `Sure! {"a":1} Hope that helps.`, `{"a":1}`, true},
		{"array", `[1,2,3]`, `[1,2,3]`, true},
		{"nested braces", `{"a":{"b":[{"c":2}]}}`, `{"a":{"b":[{"c":2}]}}`, true},
		// A brace inside a string must not close the object early.
		{"brace in string", `{"a":"}"}`, `{"a":"}"}`, true},
		{"escaped quote in string", `{"a":"say \"}\" ok"}`, `{"a":"say \"}\" ok"}`, true},
		{"no json", `I could not find anything.`, "", false},
	}
	for _, c := range cases {
		got, ok := ExtractJSON(c.in)
		if ok != c.ok || got != c.want {
			t.Fatalf("%s: ExtractJSON(%q) = (%q, %v), want (%q, %v)", c.name, c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestForceJSONRejectsTools(t *testing.T) {
	c := New("key", "model", 0)
	_, err := c.Generate(t.Context(), Request{
		ForceJSON: true,
		Tools:     []FunctionDeclaration{{Name: "x"}},
		Contents:  []Content{{Role: RoleUser, Parts: []Part{{Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("combining forced JSON with tools must be rejected before the request is sent")
	}
}

func TestRetryDelayParsing(t *testing.T) {
	payload := []byte(`{"error":{"code":429,"details":[
		{"@type":"type.googleapis.com/google.rpc.Help"},
		{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"41s"}]}}`)
	d, ok := retryDelay(payload)
	if !ok || d.Seconds() != 41 {
		t.Fatalf("retryDelay = (%v, %v), want (41s, true)", d, ok)
	}

	// A delay longer than the cap means the quota window is a day, not a
	// spike: report false so the caller fails fast instead of parking a worker.
	long := []byte(`{"error":{"details":[{"@type":"...RetryInfo","retryDelay":"3600s"}]}}`)
	if _, ok := retryDelay(long); ok {
		t.Fatal("an out-of-range retry delay must not be honoured")
	}
	if _, ok := retryDelay([]byte(`not json`)); ok {
		t.Fatal("garbage must not parse")
	}
}
