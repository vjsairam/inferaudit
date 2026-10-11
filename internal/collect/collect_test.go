package collect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKubectl writes a script that answers `get <resource>` from files in dir, and fails
// like a real API server for anything listed in missing.
func fakeKubectl(t *testing.T, dir string, missing map[string]string) string {
	t.Helper()
	var cases strings.Builder
	for res, msg := range missing {
		if err := os.WriteFile(filepath.Join(dir, res+".err"), []byte(msg+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cases.WriteString("  " + res + `) cat "` + dir + "/" + res + `.err" >&2; exit 1 ;;` + "\n")
	}
	script := `#!/bin/sh
echo "$@" >> "` + dir + `/calls.log"
[ "$1" = "config" ] && { echo test-context; exit 0; }
while [ "$1" != "get" ]; do shift; done
case "$2" in
` + cases.String() + `  *) cat "` + dir + `/$2.json" ;;
esac
`
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFromCluster(t *testing.T) {
	dir := t.TempDir()
	empty := `{"apiVersion":"v1","kind":"List","items":[]}`
	for _, src := range sources {
		os.WriteFile(filepath.Join(dir, src.resource+".json"), []byte(empty), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "pods.json"), []byte(`{"items":[{"metadata":{"name":"p","namespace":"n"}}]}`), 0o644)
	kubectl := fakeKubectl(t, dir, map[string]string{
		"scaledobjects.keda.sh": `error: the server doesn't have a resource type "scaledobjects"`,
		"nodes":                 `Error from server (Forbidden): nodes is forbidden`,
	})
	save := filepath.Join(dir, "saved")

	snap, err := FromCluster(Options{Kubectl: kubectl, Namespace: "n", SaveDir: save})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Pods) != 1 || snap.Meta.Context != "test-context" || snap.Meta.Namespace != "n" {
		t.Errorf("snapshot = %+v", snap.Meta)
	}
	if len(snap.Meta.Gaps) != 1 || !strings.Contains(snap.Meta.Gaps[0], "GPU capacity") {
		t.Errorf("gaps = %q, want only the forbidden nodes read (missing KEDA is fine)", snap.Meta.Gaps)
	}

	calls, _ := os.ReadFile(filepath.Join(dir, "calls.log"))
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if line != "config current-context" && !strings.HasPrefix(line, "--context test-context get ") {
			t.Errorf("kubectl was asked to do something other than read the pinned context: %q", line)
		}
		if strings.Contains(line, " get pods ") && !strings.Contains(line, "--namespace n") {
			t.Errorf("namespace not passed: %q", line)
		}
	}

	if !snap.Known["scaledobjects.json"] || snap.Known["nodes.json"] {
		t.Errorf("known = %v: a CRD that isn't installed is known to be empty, a forbidden read isn't", snap.Known)
	}

	again, err := FromDir(save)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Pods) != 1 || again.Meta.Context != "test-context" || again.Meta.Source != "snapshot" || len(again.Meta.Gaps) != 1 {
		t.Errorf("saved snapshot didn't round-trip: %+v", again.Meta)
	}
	if info, err := os.Stat(filepath.Join(save, "pods.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("saved files should be private: %v %v", info.Mode(), err)
	}

	if _, err := FromCluster(Options{Kubectl: kubectl, SaveDir: save}); err == nil || !strings.Contains(err.Error(), "isn't empty") {
		t.Errorf("saving over an earlier snapshot: err = %v", err)
	}
}

func TestCheckList(t *testing.T) {
	pods := sources[0]
	for _, body := range []string{
		`{}`, `null`, `{"kind":"Pod","metadata":{"name":"p"}}`,
		`{"items":[null]}`, `{"items":[{"kind":"Node","metadata":{"name":"n"}}]}`, `{"items":[{"kind":"Pod","metadata":{}}]}`,
	} {
		if err := checkList([]byte(body), pods); err == nil {
			t.Errorf("%s was accepted as a pod list", body)
		}
	}
	for _, body := range []string{`{"items":[]}`, `{"items":[{"metadata":{"name":"p"}}]}`, `{"items":[{"kind":"Pod","metadata":{"name":"p"}}]}`} {
		if err := checkList([]byte(body), pods); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
}

func TestNoContext(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'error: current-context is not set' >&2\nexit 1\n"
	kubectl := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(kubectl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := FromCluster(Options{Kubectl: kubectl}); err == nil || !strings.Contains(err.Error(), "pass --context") {
		t.Errorf("err = %v, want a request for --context", err)
	}
}

func TestFromClusterNeedsPods(t *testing.T) {
	dir := t.TempDir()
	kubectl := fakeKubectl(t, dir, map[string]string{"pods": "error: You must be logged in to the server (Unauthorized)"})
	if _, err := FromCluster(Options{Kubectl: kubectl}); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("err = %v, want the kubectl error", err)
	}
}
