// SPDX-License-Identifier: GPL-3.0-or-later

package transcript

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/joncbenderkh/cc-proxy/internal/sse"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPrompt(t *testing.T) {
	tests := []struct {
		name    string
		request string
		want    string
		wantErr bool
	}{
		{"string content", `{"messages":[{"role":"user","content":"hi"}]}`, `[{"type":"text","text":"hi"}]`, false},
		{"only the last message", `{"messages":[{"role":"user","content":"old"},{"role":"assistant","content":"a"},{"role":"user","content":"new"}]}`,
			`[{"type":"text","text":"new"}]`, false},
		{"reminders dropped, tool results kept",
			`{"messages":[{"role":"user","content":[` +
				`{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"}]},` +
				`{"type":"tool_result","tool_use_id":"t2","content":"boom","is_error":true},` +
				`{"type":"text","text":"  <system-reminder>noise</system-reminder>"},` +
				`{"type":"image","source":{}},` +
				`{"type":"text","text":"go on"}]}]}`,
			`[{"type":"tool_result","text":"ok"},{"type":"tool_result","text":"boom","is_error":true},{"type":"image"},{"type":"text","text":"go on"}]`, false},
		{"assistant prefill", `{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":"{"}]}`, `null`, false},
		{"no messages", `{"messages":[]}`, `null`, true},
		{"not json", `nope`, `null`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Prompt([]byte(tt.request))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if s := mustJSON(t, got); s != tt.want {
				t.Fatalf("got  %s\nwant %s", s, tt.want)
			}
		})
	}
}

func TestReplyJSON(t *testing.T) {
	got, err := ReplyJSON([]byte(`{"type":"message","content":[` +
		`{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Running it."},` +
		`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"type":"text","text":"Running it."},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]`
	if s := mustJSON(t, got); s != want {
		t.Fatalf("got  %s\nwant %s", s, want)
	}
}

func TestReplyStream(t *testing.T) {
	stream := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hel"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t1","name":"Read","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"/a.go\"}"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"t2","name":"Cut","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"trunc"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":9,"delta":{"type":"text_delta","text":"stray"}}` + "\n\n"

	got, err := ReplyStream(sse.Parse([]byte(stream)))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"type":"text","text":"Hello"},{"type":"tool_use","name":"Read","input":{"file_path":"/a.go"}},{"type":"tool_use","name":"Cut","input":{}}]`
	if s := mustJSON(t, got); s != want {
		t.Fatalf("got  %s\nwant %s", s, want)
	}
	if _, err := ReplyStream([]sse.Event{{Name: "content_block_start", Data: "{"}}); err == nil {
		t.Fatal("malformed event accepted")
	}
}

func TestTruncateKeepsRunesWhole(t *testing.T) {
	long := strings.Repeat("a", MaxText-1) + "é" + "tail"
	got := truncate(long)
	if !strings.HasSuffix(got, "a…") || len(got) > MaxText+len("…") {
		t.Fatalf("truncate cut badly: …%q", got[len(got)-8:])
	}
	if short := "é"; truncate(short) != short {
		t.Fatal("short text changed")
	}
}
