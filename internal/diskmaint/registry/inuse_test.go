package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// digestN is a manifest digest from a repeated hex digit.
func digestN(b byte) string { return "sha256:" + repeatByte(b, 64) }

// fakeRunner answers every command with out and err, and records each argv and
// the environment it was sent.
type fakeRunner struct {
	out   []byte
	err   error
	calls [][]string
	envs  [][]string
}

func (f *fakeRunner) run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	f.envs = append(f.envs, slices.Clone(env))
	return f.out, f.err
}

// recordsDir returns a readable sandbox-records directory holding files.
func recordsDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write record %s: %v", name, err)
		}
	}
	return dir
}

// podJSON is one pod with a single container: image is what the spec and the
// status name, imageID what the node reports.
func podJSON(image, imageID string) string {
	return `{"spec":{"containers":[{"image":"` + image + `"}]},` +
		`"status":{"containerStatuses":[{"image":"` + image + `","imageID":"` + imageID + `"}]}}`
}

func podListJSON(pods ...string) []byte {
	return []byte(`{"items":[` + strings.Join(pods, ",") + `]}`)
}

// assertSet fails unless got holds exactly want.
func assertSet(t *testing.T, what string, got map[string]bool, want ...string) {
	t.Helper()
	for _, w := range want {
		if !got[w] {
			t.Errorf("%s missing %q (got %v)", what, w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestReadInUseCollectsDigestsFromEachImageIDShape(t *testing.T) {
	t.Parallel()
	image := "localhost:5001/gastown-polecat:2d5a48dcb"
	bare := digestN('c')
	run := &fakeRunner{out: podListJSON(
		podJSON(image, "localhost:5001/gastown-polecat@"+digestN('a')),
		podJSON(image, "docker-pullable://localhost:5001/gastown-polecat@"+digestN('b')),
		podJSON(image, bare),
	)}

	got, err := ReadInUse(context.Background(), run.run, "/kubeconfig", recordsDir(t, nil))
	if err != nil {
		t.Fatalf("ReadInUse: %v", err)
	}
	assertSet(t, "Digests", got.Digests, digestN('a'), digestN('b'))
	if got.Digests[bare] {
		t.Errorf("Digests holds the bare config digest %q; it names no manifest", bare)
	}
	// The bare config digest is ignored for Digests on purpose (plan, Task 4
	// step 1): it names no manifest, so keeping it would keep nothing, and it
	// names no repository either: "sha256" is not a repo.
	assertSet(t, "Repos", got.Repos, "gastown-polecat")
	if len(run.calls) != 1 || strings.Join(run.calls[0], " ") != "kubectl --kubeconfig /kubeconfig get pods -A -o json" {
		t.Errorf("ran %q, want kubectl --kubeconfig /kubeconfig get pods -A -o json", run.calls)
	}
}

func TestReadInUseCollectsTagsFromImageFields(t *testing.T) {
	t.Parallel()
	pods := []byte(`{"items":[{"spec":{` +
		`"containers":[{"image":"localhost:5001/gastown-polecat:2d5a48dcb"}],` +
		`"initContainers":[{"image":"localhost:5001/seedtest/polecat:init1"}]}}]}`)
	run := &fakeRunner{out: pods}

	got, err := ReadInUse(context.Background(), run.run, "/kubeconfig", recordsDir(t, nil))
	if err != nil {
		t.Fatalf("ReadInUse: %v", err)
	}
	assertSet(t, "Tags", got.Tags, "gastown-polecat:2d5a48dcb", "seedtest/polecat:init1")
	assertSet(t, "Repos", got.Repos, "gastown-polecat", "seedtest/polecat")
}

func TestReadInUseCollectsReposFromRefs(t *testing.T) {
	t.Parallel()
	run := &fakeRunner{out: podListJSON(podJSON("localhost:5001/seedtest/polecat:x", ""))}

	got, err := ReadInUse(context.Background(), run.run, "/kubeconfig", recordsDir(t, nil))
	if err != nil {
		t.Fatalf("ReadInUse: %v", err)
	}
	assertSet(t, "Repos", got.Repos, "seedtest/polecat")
	assertSet(t, "Tags", got.Tags, "seedtest/polecat:x")
	if len(got.Digests) != 0 {
		t.Errorf("Digests = %v, want none", got.Digests)
	}
}

func TestReadInUseReadsSandboxRecords(t *testing.T) {
	t.Parallel()
	dir := recordsDir(t, map[string]string{
		"s-1.json":  `{"session":"s-1","image":"localhost:5001/seedtest/polecat:rec1"}`,
		"notes.txt": "the sandbox pulled localhost:5001/gastown-polecat:rec2\n",
	})
	// A nested directory is not a record: only the directory's own files count.
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "x.json"), []byte(`"localhost:5001/stray/repo:old"`), 0o644); err != nil {
		t.Fatal(err)
	}
	run := &fakeRunner{out: podListJSON()}

	got, err := ReadInUse(context.Background(), run.run, "/kubeconfig", dir)
	if err != nil {
		t.Fatalf("ReadInUse: %v", err)
	}
	assertSet(t, "Tags", got.Tags, "seedtest/polecat:rec1", "gastown-polecat:rec2")
	assertSet(t, "Repos", got.Repos, "seedtest/polecat", "gastown-polecat")
}

func TestReadInUseReadsRefsWithoutHostPort(t *testing.T) {
	t.Parallel()
	dir := recordsDir(t, map[string]string{
		"s-1.json":  `{"image":"gastown-polecat@` + digestN('d') + `"}`,
		"notes.txt": "the sandbox pulled seedtest/polecat:rec3\n",
	})
	run := &fakeRunner{out: podListJSON()}

	got, err := ReadInUse(context.Background(), run.run, "/kubeconfig", dir)
	if err != nil {
		t.Fatalf("ReadInUse: %v", err)
	}
	assertSet(t, "Digests", got.Digests, digestN('d'))
	assertSet(t, "Tags", got.Tags, "seedtest/polecat:rec3")
	assertSet(t, "Repos", got.Repos, "gastown-polecat", "seedtest/polecat")
}

func TestReadInUseReadsSlashlessTaggedRefInRecord(t *testing.T) {
	t.Parallel()
	// A record may name an image without a host, as the sandbox config does
	// ("gastown-polecat:dev"), so a slash-less repo:tag is a reference.
	dir := recordsDir(t, map[string]string{
		"notes.txt": "the sandbox pulled gastown-polecat:rec5\n",
	})
	run := &fakeRunner{out: podListJSON()}

	got, err := ReadInUse(context.Background(), run.run, "/kubeconfig", dir)
	if err != nil {
		t.Fatalf("ReadInUse: %v", err)
	}
	assertSet(t, "Tags", got.Tags, "gastown-polecat:rec5")
	assertSet(t, "Repos", got.Repos, "gastown-polecat")
	assertSet(t, "Digests", got.Digests)
}

func TestReadInUseIgnoresURLsAndPathsInRecords(t *testing.T) {
	t.Parallel()
	// Records hold more than images: a URL is a location and a path is not a
	// repository, so neither is a reference. Parsing them as one would refuse
	// a record that is perfectly valid.
	dir := recordsDir(t, map[string]string{
		"s-1.json": `{"session":"s-1","image":"localhost:5001/gastown-polecat:rec4",` +
			`"note":"see https://forgejo.internal:3000/sloan/gastown/src/x.json",` +
			`"artifact":"/var/lib/gastown/export.tar",` +
			`"policy":"~/gt/.runtime/sandboxes/s-1.json"}`,
	})
	run := &fakeRunner{out: podListJSON()}

	got, err := ReadInUse(context.Background(), run.run, "/kubeconfig", dir)
	if err != nil {
		t.Fatalf("ReadInUse refused a record naming a URL and a path: %v", err)
	}
	assertSet(t, "Tags", got.Tags, "gastown-polecat:rec4")
	assertSet(t, "Repos", got.Repos, "gastown-polecat")
	assertSet(t, "Digests", got.Digests)
}

func TestReadInUseFailsClosedOnKubectlError(t *testing.T) {
	t.Parallel()
	run := &fakeRunner{err: errors.New("connection refused")}

	_, err := ReadInUse(context.Background(), run.run, "/kubeconfig", recordsDir(t, nil))
	if err == nil {
		t.Fatal("ReadInUse succeeded with a kubectl error; want an error")
	}
	if !strings.Contains(err.Error(), "kubectl") {
		t.Errorf("error %q does not name kubectl", err)
	}
}

func TestReadInUseFailsClosedOnBadJSON(t *testing.T) {
	t.Parallel()
	replies := map[string][]byte{
		"truncated": []byte(`{"items":[` + "\n" + `"not a pod"`),
		"null":      []byte(`null`),
	}
	for name, out := range replies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := &fakeRunner{out: out}
			_, err := ReadInUse(context.Background(), run.run, "/kubeconfig", recordsDir(t, nil))
			if err == nil {
				t.Fatalf("ReadInUse succeeded on the %s reply; want an error", name)
			}
		})
	}
}

func TestReadInUseMissingPodListIsAnError(t *testing.T) {
	t.Parallel()
	replies := map[string][]byte{
		"no items key": []byte(`{"kind":"PodList"}`),
		"null items":   []byte(`{"items":null}`),
		"empty object": []byte(`{}`),
	}
	for name, out := range replies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := &fakeRunner{out: out}
			_, err := ReadInUse(context.Background(), run.run, "/kubeconfig", recordsDir(t, nil))
			if err == nil {
				t.Fatalf("ReadInUse succeeded on the reply with %s; want an error", name)
			}
		})
	}
}

func TestReadInUseMissingRecordsDirIsAnError(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "absent")
	run := &fakeRunner{out: podListJSON()}

	_, err := ReadInUse(context.Background(), run.run, "/kubeconfig", dir)
	if err == nil {
		t.Fatal("ReadInUse succeeded with a missing records directory; want an error")
	}
}

func TestReadInUseUnreadableRecordsDirIsAnError(t *testing.T) {
	t.Parallel()
	// A regular file where the directory should be: readable enough to stat,
	// so only opening it as a directory fails.
	path := filepath.Join(t.TempDir(), "records")
	if err := os.WriteFile(path, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := &fakeRunner{out: podListJSON()}

	_, err := ReadInUse(context.Background(), run.run, "/kubeconfig", path)
	if err == nil {
		t.Fatal("ReadInUse succeeded with an unreadable records directory; want an error")
	}
}

func TestReadInUseUnreadableRecordIsAnError(t *testing.T) {
	t.Parallel()
	dir := recordsDir(t, nil)
	// A dangling symlink opens as a record and fails: a record this run cannot
	// read must refuse the read.
	if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "dangling.json")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	run := &fakeRunner{out: podListJSON()}

	_, err := ReadInUse(context.Background(), run.run, "/kubeconfig", dir)
	if err == nil {
		t.Fatal("ReadInUse succeeded with an unreadable record; want an error")
	}
}

func TestReadInUseDropsNamesFailingValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		out   []byte
		files map[string]string
	}{
		{
			name: "bad repo in a pod image",
			out:  podListJSON(podJSON("localhost:5001/Bad_Repo:x", "")),
		},
		{
			name:  "bad tag in a sandbox record",
			out:   podListJSON(),
			files: map[string]string{"s-1.json": `{"image":"localhost:5001/gastown-polecat:` + repeatByte('a', 129) + `"}`},
		},
		{
			name:  "malformed digest in a sandbox record",
			out:   podListJSON(),
			files: map[string]string{"s-1.json": `{"image":"localhost:5001/gastown-polecat@sha256:nothex"}`},
		},
		{
			name:  "malformed digest in a sandbox record with no host port",
			out:   podListJSON(),
			files: map[string]string{"s-1.json": `{"image":"gastown-polecat@sha256:nothex"}`},
		},
		{
			name: "bad digest in a pod image ID",
			out:  podListJSON(podJSON("localhost:5001/gastown-polecat:x", "localhost:5001/gastown-polecat@sha256:nothex")),
		},
		{
			name: "bad bare digest in a pod image ID",
			out:  podListJSON(podJSON("localhost:5001/gastown-polecat:x", "sha256:"+repeatByte('g', 64))),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := &fakeRunner{out: tc.out}
			_, err := ReadInUse(context.Background(), run.run, "/kubeconfig", recordsDir(t, tc.files))
			if !errors.Is(err, ErrBadName) {
				t.Fatalf("ReadInUse error = %v, want ErrBadName", err)
			}
		})
	}
}
