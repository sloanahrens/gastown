package gocacheprog_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/gocacheprog"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

func TestReader(t *testing.T) {
	t.Parallel()
	body := []byte("hello cache")
	stream := strings.Join([]string{
		`{"ID":1,"Command":"get","ActionID":"a2V5"}`,
		`{"ID":2,"Command":"put","ActionID":"a2V5","OutputID":"b3V0","BodySize":11}`,
		`"` + base64.StdEncoding.EncodeToString(body) + `"`,
		`{"ID":3,"Command":"put","ActionID":"a2V5"}`,
		`{"ID":4,"Command":"get2"}`,
		`{"ID":5,"Command":"close"}`,
	}, "\n") + "\n"
	r := gocacheprog.NewReader(strings.NewReader(stream))
	want := []*gocacheprog.Request{
		{ID: 1, Command: gocacheprog.CmdGet, ActionID: []byte("key")},
		{ID: 2, Command: gocacheprog.CmdPut, ActionID: []byte("key"), OutputID: []byte("out"), BodySize: 11, Body: body},
		{ID: 3, Command: gocacheprog.CmdPut, ActionID: []byte("key")},
		{ID: 4, Command: "get2"},
		{ID: 5, Command: gocacheprog.CmdClose},
	}
	for i, w := range want {
		if got, err := r.Next(); err != nil || !reflect.DeepEqual(got, w) {
			t.Fatalf("Next #%d = %+v, %v; want %+v", i, got, err, w)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Errorf("Next after the last request = %v, want io.EOF", err)
	}
}

// want is the message each refusal must carry, so a guard that stops firing
// fails its row.
func TestReaderErrors(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in, want string }{
		{"cut object", `{"ID":1,"Command":"get"`, "cut request"},
		{"malformed object", "{\"ID\":1,\n", "malformed request"},
		{"body missing", "{\"ID\":42,\"Command\":\"put\",\"BodySize\":3}\n", "request 42: body: unexpected EOF"},
		{"body not a string", "{\"ID\":42,\"Command\":\"put\",\"BodySize\":3}\n123\n", "request 42: body is not a JSON string"},
		{"body not base64", "{\"ID\":42,\"Command\":\"put\",\"BodySize\":3}\n\"abc\"\n", "request 42: body is not base64"},
		{"body too short", "{\"ID\":42,\"Command\":\"put\",\"BodySize\":3}\n\"aGk=\"\n", "request 42: body is 2 bytes, want 3"},
		{"negative size", "{\"ID\":42,\"Command\":\"put\",\"BodySize\":-1}\n", "request 42: negative body size -1"},
		{"size over limit", "{\"ID\":42,\"Command\":\"put\",\"BodySize\":1073741825}\n", "request 42: body size 1073741825 over limit 1073741824"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := gocacheprog.NewReader(strings.NewReader(tt.in)).Next()
			if err == nil || errors.Is(err, io.EOF) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Next = %v, want an error carrying %q", err, tt.want)
			}
		})
	}
}

func TestResponseJSON(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b, err := json.Marshal(gocacheprog.Response{ID: 1, Err: "e", KnownCommands: []gocacheprog.Cmd{gocacheprog.CmdGet}, Miss: true, OutputID: []byte("o"), Size: 2, Time: &now, DiskPath: "/d"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ID":1,"Err":"e","KnownCommands":["get"],"Miss":true,"OutputID":"bw==","Size":2,"Time":"2026-01-01T00:00:00Z","DiskPath":"/d"}`
	if string(b) != want {
		t.Errorf("Response JSON = %s, want %s", b, want)
	}
}
