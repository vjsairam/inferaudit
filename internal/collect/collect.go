// Package collect reads cluster state with `kubectl get`, or loads a snapshot saved earlier.
// It only ever runs get commands, so the report needs read access and nothing more.
package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vjsairam/inferaudit/internal/kube"
)

// A source is one `kubectl get` call and the file its output is saved to.
type source struct {
	file      string
	kind      string // what each item must be, when it says
	resource  string
	cluster   bool // cluster-scoped, so no namespace flags
	optional  bool // a CRD that may not be installed
	needed    bool // the report is meaningless without it
	whatFails string
}

var sources = []source{
	{file: "pods.json", kind: "Pod", resource: "pods", needed: true},
	{file: "replicasets.json", kind: "ReplicaSet", resource: "replicasets.apps", whatFails: "pods can't be traced to their Deployments"},
	{file: "deployments.json", kind: "Deployment", resource: "deployments.apps", whatFails: "Deployment replica counts are unknown"},
	{file: "statefulsets.json", kind: "StatefulSet", resource: "statefulsets.apps", whatFails: "StatefulSet replica counts are unknown"},
	{file: "services.json", kind: "Service", resource: "services", whatFails: "Service exposure isn't checked"},
	{file: "poddisruptionbudgets.json", kind: "PodDisruptionBudget", resource: "poddisruptionbudgets.policy", whatFails: "disruption budgets aren't checked"},
	{file: "horizontalpodautoscalers.json", kind: "HorizontalPodAutoscaler", resource: "horizontalpodautoscalers.autoscaling", whatFails: "autoscaler settings are unknown"},
	{file: "events.json", kind: "Event", resource: "events", whatFails: "image pull times are unknown"},
	{file: "nodes.json", kind: "Node", resource: "nodes", cluster: true, whatFails: "GPU capacity per node is unknown"},
	{file: "scaledobjects.json", kind: "ScaledObject", resource: "scaledobjects.keda.sh", optional: true, whatFails: "KEDA settings are unknown"},
}

const (
	requestTimeout = "60s"            // passed to kubectl for each API request
	commandTimeout = 90 * time.Second // backstop for a kubectl call that hangs
)

// Snapshot is everything the audit reads.
type Snapshot struct {
	Meta          Meta
	Known         map[string]bool // snapshot files that were read, even if empty
	Pods          []kube.Pod
	ReplicaSets   []kube.ReplicaSet
	Workloads     []kube.Workload
	Services      []kube.Service
	PDBs          []kube.PodDisruptionBudget
	HPAs          []kube.HorizontalPodAutoscaler
	ScaledObjects []kube.ScaledObject
	Events        []kube.Event
	Nodes         []kube.Node
}

// Meta records where a snapshot came from and what couldn't be read.
type Meta struct {
	Source        string    `json:"source"` // "cluster" or "snapshot"
	Context       string    `json:"context,omitempty"`
	Namespace     string    `json:"namespace,omitempty"`
	AllNamespaces bool      `json:"allNamespaces"`
	CollectedAt   time.Time `json:"collectedAt,omitzero"`
	Note          string    `json:"note,omitempty"`
	Gaps          []string  `json:"gaps,omitempty"`
}

// Options control a live collection.
type Options struct {
	Kubectl   string // path to kubectl; "kubectl" if empty
	Context   string
	Namespace string // empty means all namespaces
	SaveDir   string // if set, raw outputs and meta.json are written here; it must be new or empty
}

// FromCluster runs one `kubectl get -o json` per resource type.
func FromCluster(opts Options) (*Snapshot, error) {
	kubectl := opts.Kubectl
	if kubectl == "" {
		kubectl = "kubectl"
	}
	// Pin the context once so every read goes to the same cluster, even if the current
	// context changes while this runs. Inside a pod there may be no context, which is fine.
	if opts.Context == "" {
		ctx, err := run(kubectl, "config", "current-context")
		switch {
		case err == nil:
			opts.Context = strings.TrimSpace(string(ctx))
		case os.Getenv("KUBERNETES_SERVICE_HOST") == "":
			return nil, fmt.Errorf("couldn't read the current kubectl context (%s); pass --context", firstLine(err))
		}
	}
	meta := Meta{
		Source: "cluster", Context: opts.Context, Namespace: opts.Namespace,
		AllNamespaces: opts.Namespace == "", CollectedAt: time.Now().UTC(),
	}
	if opts.SaveDir != "" {
		if err := emptyDir(opts.SaveDir); err != nil {
			return nil, err
		}
	}

	raw := map[string][]byte{}
	for _, src := range sources {
		args := []string{"get", src.resource, "-o", "json", "--request-timeout", requestTimeout}
		if !src.cluster {
			if opts.Namespace == "" {
				args = append(args, "--all-namespaces")
			} else {
				args = append(args, "--namespace", opts.Namespace)
			}
		}
		if opts.Context != "" {
			args = append([]string{"--context", opts.Context}, args...)
		}
		out, err := run(kubectl, args...)
		switch {
		case err == nil:
		case src.optional && notInstalled(err):
			out = []byte(`{"apiVersion":"v1","kind":"List","items":[]}` + "\n") // not installed, so known to be empty
		case src.needed:
			return nil, fmt.Errorf("kubectl get %s: %w", src.resource, err)
		default:
			meta.Gaps = append(meta.Gaps, fmt.Sprintf("couldn't read %s, so %s (%s)", src.resource, src.whatFails, firstLine(err)))
			continue
		}
		raw[src.file] = out
		if opts.SaveDir != "" {
			if err := os.WriteFile(filepath.Join(opts.SaveDir, src.file), out, 0o600); err != nil {
				return nil, err
			}
		}
	}
	if opts.SaveDir != "" {
		b, _ := json.MarshalIndent(meta, "", "  ")
		if err := os.WriteFile(filepath.Join(opts.SaveDir, "meta.json"), b, 0o600); err != nil {
			return nil, err
		}
	}
	return decode(raw, meta)
}

// FromDir loads a snapshot saved with --save, or any folder holding the same files.
func FromDir(dir string) (*Snapshot, error) {
	var meta Meta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &meta); err != nil {
			return nil, fmt.Errorf("meta.json: %w", err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	meta.Source = "snapshot"

	raw := map[string][]byte{}
	for _, src := range sources {
		b, err := os.ReadFile(filepath.Join(dir, src.file))
		if errors.Is(err, os.ErrNotExist) {
			if src.needed {
				return nil, fmt.Errorf("%s is missing from %s", src.file, dir)
			}
			if !gapRecorded(meta.Gaps, src.resource) {
				meta.Gaps = append(meta.Gaps, fmt.Sprintf("no %s in the snapshot, so %s", src.file, src.whatFails))
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		raw[src.file] = b
	}
	return decode(raw, meta)
}

func decode(raw map[string][]byte, meta Meta) (*Snapshot, error) {
	s := &Snapshot{Meta: meta, Known: map[string]bool{}}
	for _, src := range sources {
		if b, ok := raw[src.file]; ok {
			if err := checkList(b, src); err != nil {
				return nil, err
			}
			s.Known[src.file] = true
		}
	}
	var err error
	if s.Pods, err = items[kube.Pod](raw, "pods.json"); err != nil {
		return nil, err
	}
	if s.ReplicaSets, err = items[kube.ReplicaSet](raw, "replicasets.json"); err != nil {
		return nil, err
	}
	deployments, err := items[kube.Workload](raw, "deployments.json")
	if err != nil {
		return nil, err
	}
	statefulsets, err := items[kube.Workload](raw, "statefulsets.json")
	if err != nil {
		return nil, err
	}
	for i := range deployments {
		deployments[i].Kind = "Deployment"
	}
	for i := range statefulsets {
		statefulsets[i].Kind = "StatefulSet"
	}
	s.Workloads = append(deployments, statefulsets...)
	if s.Services, err = items[kube.Service](raw, "services.json"); err != nil {
		return nil, err
	}
	if s.PDBs, err = items[kube.PodDisruptionBudget](raw, "poddisruptionbudgets.json"); err != nil {
		return nil, err
	}
	if s.HPAs, err = items[kube.HorizontalPodAutoscaler](raw, "horizontalpodautoscalers.json"); err != nil {
		return nil, err
	}
	if s.ScaledObjects, err = items[kube.ScaledObject](raw, "scaledobjects.json"); err != nil {
		return nil, err
	}
	if s.Events, err = items[kube.Event](raw, "events.json"); err != nil {
		return nil, err
	}
	if s.Nodes, err = items[kube.Node](raw, "nodes.json"); err != nil {
		return nil, err
	}
	return s, nil
}

// checkList rejects anything that isn't a `kubectl get -o json` list of the expected kind, so
// a wrong file can't pass for an empty cluster.
func checkList(b []byte, src source) error {
	var list struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("%s: %w", src.file, err)
	}
	if payload := bytes.TrimSpace(list.Items); len(payload) == 0 || payload[0] != '[' {
		return fmt.Errorf("%s: expected a kubectl list with an items array", src.file)
	}
	var objs []*struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(list.Items, &objs); err != nil {
		return fmt.Errorf("%s: %w", src.file, err)
	}
	for i, o := range objs {
		switch {
		case o == nil || o.Metadata.Name == "":
			return fmt.Errorf("%s: item %d has no name", src.file, i)
		case o.Kind != "" && o.Kind != src.kind:
			return fmt.Errorf("%s: item %d is a %s, expected %s", src.file, i, o.Kind, src.kind)
		}
	}
	return nil
}

func items[T any](raw map[string][]byte, file string) ([]T, error) {
	b, ok := raw[file]
	if !ok {
		return nil, nil
	}
	var list kube.List[T]
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return list.Items, nil
}

// emptyDir creates dir, private to the user, or checks that it exists and is empty, so a
// new snapshot can't mix with files from an earlier one.
func emptyDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("--save folder %s isn't empty", dir)
	}
	return os.Chmod(dir, 0o700)
}

func gapRecorded(gaps []string, resource string) bool {
	for _, g := range gaps {
		if strings.Contains(g, " "+resource+",") {
			return true
		}
	}
	return false
}

func run(kubectl string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, kubectl, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second // don't wait on pipes held open by kubectl's children
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return stdout.Bytes(), nil
}

func notInstalled(err error) bool {
	return strings.Contains(err.Error(), "the server doesn't have a resource type")
}

func firstLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}
