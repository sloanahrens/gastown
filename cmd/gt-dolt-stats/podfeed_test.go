package main

import (
	"reflect"
	"testing"
)

// TestPodAppendArgv: an absolute path becomes the kubectl exec argv that
// appends to it, and a path that is not absolute or that sh cannot quote is
// refused (gt-deyva8).
func TestPodAppendArgv(t *testing.T) {
	t.Parallel()
	const path = "/Users/sloan/gt/.runtime/metrics/resources/2026-11-25.jsonl"
	tests := []struct {
		name    string
		path    string
		want    []string
		wantErr bool
	}{
		{
			name: "absolute path",
			path: path,
			want: []string{
				"kubectl", "--context", "colima-gt-control", "-n", "gt", "exec", "-i",
				"deploy/daemon", "-c", "daemon", "--", "sh", "-c", "cat >> '" + path + "'",
			},
		},
		{name: "relative path", path: "metrics/resources/2026-11-25.jsonl", wantErr: true},
		{name: "single quote", path: "/Users/sloan/gt/it's.jsonl", wantErr: true},
		{name: "newline", path: "/Users/sloan/gt/a\nb.jsonl", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := podAppendArgv("colima-gt-control", "gt", tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("podAppendArgv(%q) = %v, nil: want an error", tt.path, got)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("podAppendArgv(%q) = %v, %v: want %v", tt.path, got, err, tt.want)
			}
		})
	}
}
