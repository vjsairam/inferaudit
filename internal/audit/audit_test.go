package audit

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vjsairam/inferaudit/internal/collect"
	"github.com/vjsairam/inferaudit/internal/kube"
)

func ids(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.ID)
	}
	sort.Strings(out)
	return out
}

// The lab snapshot is rebuilt from a real run, so its numbers are known.
func TestLabSnapshot(t *testing.T) {
	snap, err := collect.FromDir("../../testdata/lab-2026-08-23")
	if err != nil {
		t.Fatal(err)
	}
	r := Run(snap, Options{})

	if len(r.Workloads) != 1 || r.Workloads[0].Name != "vllm" || r.Workloads[0].Engine != "vLLM" {
		t.Fatalf("workloads = %+v", r.Workloads)
	}
	w := r.Workloads[0]
	if w.Replicas != "1 to 2" || !strings.HasPrefix(w.Autoscaler, "KEDA") {
		t.Errorf("scale = %q from %q", w.Replicas, w.Autoscaler)
	}
	if got, want := r.Totals.SlowestStart, 7*time.Minute+40*time.Second; got != want {
		t.Errorf("slowest start = %s, want %s", got, want)
	}
	if got, want := r.Totals.SlowestPull, 4*time.Minute+38286*time.Millisecond; got != want {
		t.Errorf("slowest pull = %s, want %s", got, want)
	}

	scaled := w.Timelines[1]
	if scaled.Pod != "vllm-5f577ccf85-j27ct" || scaled.StartToReady() != 2*time.Minute+56*time.Second || scaled.ImageBytes != 9110698465 {
		t.Errorf("second replica timeline = %+v", scaled)
	}
	if first := w.Timelines[0]; first.Total() != 0 || first.State != "Running" {
		t.Errorf("first replica has no recorded Ready time, got total %s state %q", first.Total(), first.State)
	}

	want := []string{"reliability/single-replica", "startup/model-cache-not-kept", "startup/slow-image-pull", "startup/slow-start-to-ready"}
	if got := ids(r.Findings); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("findings = %v, want %v", got, want)
	}
	if r.Totals.GPUsAllocatable != 2 || r.Totals.GPUsRequested != 2 {
		t.Errorf("GPUs = %d of %d", r.Totals.GPUsRequested, r.Totals.GPUsAllocatable)
	}
}

func TestVLLMArgs(t *testing.T) {
	tests := []struct {
		name      string
		c         kube.Container
		model     string
		revision  bool
		apiKeyArg bool
	}{
		{"serve positional", kube.Container{Command: []string{"vllm", "serve"}, Args: []string{"meta-llama/Llama-3.1-8B-Instruct", "--max-model-len", "8192"}},
			"meta-llama/Llama-3.1-8B-Instruct", false, false},
		{"--model flag", kube.Container{Image: "vllm/vllm-openai:v0.27.1", Args: []string{"--model", "Qwen/Qwen2.5-7B-Instruct", "--revision", "abc", "--enable-prefix-caching", "--api-key", "$(KEY)"}},
			"Qwen/Qwen2.5-7B-Instruct", true, true},
		{"equals form", kube.Container{Command: []string{"python3", "-m", "vllm.entrypoints.openai.api_server"}, Args: []string{"--model=Qwen/Qwen2.5-7B-Instruct", "--revision=abc"}},
			"Qwen/Qwen2.5-7B-Instruct", true, false},
		{"empty revision doesn't count", kube.Container{Image: "vllm/vllm-openai@sha256:abc", Args: []string{"--model", "Qwen/Qwen2.5-7B-Instruct", "--revision="}},
			"Qwen/Qwen2.5-7B-Instruct", false, false},
	}
	for _, tt := range tests {
		a := vllmArgs(tt.c)
		if !a.known || a.model != tt.model || a.has("--revision") != tt.revision || a.has("--api-key") != tt.apiKeyArg {
			t.Errorf("%s: got model %q flags %v known %v", tt.name, a.model, a.flags, a.known)
		}
	}
	for name, c := range map[string]kube.Container{
		"shell":        {Command: []string{"/bin/sh", "-c", "vllm serve mistralai/Mistral-7B-v0.3"}},
		"env wrapper":  {Command: []string{"/usr/bin/env", "sh", "-c", "vllm serve mistralai/Mistral-7B-v0.3"}},
		"start script": {Command: []string{"/app/start.sh"}, Args: []string{"--model", "mistralai/Mistral-7B-v0.3"}},
		"custom image": {Image: "acme/llm-server:1", Args: []string{"--model", "mistralai/Mistral-7B-v0.3"}},
		"other module": {Command: []string{"python3", "-m", "vllm_wrapper"}, Args: []string{"--model", "mistralai/Mistral-7B-v0.3"}},
		"config":       {Command: []string{"vllm", "serve"}, Args: []string{"--config", "/etc/vllm.yaml"}},
	} {
		if vllmArgs(c).known {
			t.Errorf("%s: settings should be unknown", name)
		}
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[string]bool{
		"Qwen/Qwen2.5-7B": true, "/models/qwen": false, "s3://bucket/model": false, "./local/model": false, "gpt2": false, "$(MODEL)/x": false,
	} {
		if got := looksLikeHubID(in); got != want {
			t.Errorf("looksLikeHubID(%q) = %v", in, got)
		}
	}
	for in, want := range map[string]string{
		"vllm/vllm-openai:v0.27.1": "v0.27.1", "vllm/vllm-openai": "", "registry:5000/team/vllm": "", "registry:5000/team/vllm:latest": "latest",
	} {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"vllm/vllm-openai:v0.27.1": "vllm/vllm-openai", "vllm/vllm-openai@sha256:abc": "vllm/vllm-openai",
		"vllm/vllm-openai-custom:1": "vllm/vllm-openai-custom", "registry:5000/vllm/vllm-openai": "registry:5000/vllm/vllm-openai",
	} {
		if got := imageRepo(in); got != want {
			t.Errorf("imageRepo(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[time.Duration]string{
		52 * time.Second: "52s", 4*time.Minute + 38286*time.Millisecond: "4m 38s", 7*time.Minute + 40*time.Second: "7m 40s", 65 * time.Minute: "1h 05m",
	} {
		if got := Dur(in); got != want {
			t.Errorf("Dur(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestTimelineFromEvents(t *testing.T) {
	at := func(s string) kube.Time {
		tm, _ := time.Parse(time.RFC3339, "2026-10-11T"+s+"Z")
		return kube.Time{Time: tm}
	}
	c := kube.Container{Name: "server", Image: "vllm/vllm-openai:v0.27.1"}
	p := &kube.Pod{}
	p.Metadata.Name, p.Metadata.UID, p.Metadata.CreationTimestamp = "llm-0", "uid-new", at("10:00:00")
	p.Status.Conditions = []kube.PodCondition{{Type: "Ready", Status: "True", LastTransitionTime: at("10:03:10")}}

	ev := func(reason, msg, when, uid string) *kube.Event {
		e := &kube.Event{Reason: reason, Message: msg, LastTimestamp: at(when)}
		e.InvolvedObject.Kind, e.InvolvedObject.Name, e.InvolvedObject.UID = "Pod", "llm-0", uid
		e.InvolvedObject.FieldPath = "spec.containers{server}"
		return e
	}
	events := []*kube.Event{
		ev("Pulled", `Successfully pulled image "vllm/vllm-openai:v0.27.1" in 9m0s`, "09:00:00", "uid-old"), // earlier pod, same name
		ev("Started", "Started container server", "10:01:40", "uid-new"),
		ev("Pulling", `Pulling image "vllm/vllm-openai:v0.27.1"`, "10:00:01", "uid-new"),
		ev("Pulled", `Successfully pulled image "vllm/vllm-openai:v0.27.1" in 1m34.5s (1m34.5s including waiting). Image size: 123456789 bytes.`, "10:01:36", "uid-new"),
	}
	events = append(events,
		ev("Started", "Started container sidecar", "10:00:30", "uid-new"), // sidecar, no field path
	)
	events[len(events)-1].InvolvedObject.FieldPath = ""
	tl := buildTimeline(p, c, events)
	if tl.PullTook != 94500*time.Millisecond || tl.ImageBytes != 123456789 {
		t.Errorf("pull = %s, %d bytes", tl.PullTook, tl.ImageBytes)
	}
	if tl.StartToReady() != 90*time.Second || tl.Total() != 190*time.Second || tl.State != "Ready" {
		t.Errorf("start to ready %s, total %s, state %s", tl.StartToReady(), tl.Total(), tl.State)
	}

	cached := buildTimeline(p, c, []*kube.Event{ev("Pulled", `Container image "vllm/vllm-openai:v0.27.1" already present on machine`, "10:00:02", "uid-new")})
	if !cached.ImageCached || cached.PullTook != 0 {
		t.Errorf("cached image: %+v", cached)
	}

	// Retries of one pull fold into a single Pulling event; the pull began at its first time.
	retried := ev("Pulling", `Pulling image "vllm/vllm-openai:v0.27.1"`, "10:04:00", "uid-new")
	retried.FirstTimestamp, retried.Count = at("10:00:01"), 3
	queued := ev("Pulled", `Successfully pulled image "vllm/vllm-openai:v0.27.1" in 888ms (4.809s including waiting). Image size: 20643193 bytes.`, "10:04:05", "uid-new")
	tl = buildTimeline(p, c, []*kube.Event{queued, retried})
	if !tl.PullStart.Equal(at("10:00:01").Time) || tl.PullTook != 4809*time.Millisecond || tl.PullActive != 888*time.Millisecond {
		t.Errorf("retried pull: start %s took %s active %s", tl.PullStart, tl.PullTook, tl.PullActive)
	}

	// After a restart, Ready belongs to a later start, so no durations are claimed.
	p.Status.ContainerStatuses = []kube.ContainerStatus{{Name: "server", RestartCount: 2}}
	tl = buildTimeline(p, c, events)
	if tl.Total() != 0 || tl.StartToReady() != 0 || tl.PullTook == 0 {
		t.Errorf("restarted pod: total %s, start to ready %s, pull %s", tl.Total(), tl.StartToReady(), tl.PullTook)
	}
}

func TestOwnership(t *testing.T) {
	s := &collect.Snapshot{Known: map[string]bool{}}
	dep := kube.Workload{Kind: "Deployment"}
	dep.Metadata.Name, dep.Metadata.Namespace, dep.Metadata.UID = "llm", "ai", "dep-1"
	dep.Spec.Template.Spec.Containers = []kube.Container{{Name: "vllm", Image: "vllm/vllm-openai:v0.28.0"}}
	s.Workloads = []kube.Workload{dep}

	rs := kube.ReplicaSet{}
	rs.Metadata.Name, rs.Metadata.Namespace, rs.Metadata.UID = "llm-abc", "ai", "rs-1"
	rs.Metadata.OwnerReferences = []kube.OwnerReference{{Kind: "Deployment", Name: "llm", UID: "dep-1", Controller: true}}
	s.ReplicaSets = []kube.ReplicaSet{rs}

	pod := func(name, rsUID string, controller bool, image string) kube.Pod {
		p := kube.Pod{}
		p.Metadata.Name, p.Metadata.Namespace, p.Metadata.UID = name, "ai", "uid-"+name
		p.Metadata.OwnerReferences = []kube.OwnerReference{{Kind: "ReplicaSet", Name: "llm-abc", UID: rsUID, Controller: controller}}
		p.Spec.Containers = []kube.Container{{Name: "vllm", Image: image}}
		return p
	}
	s.Pods = []kube.Pod{
		pod("current", "rs-1", true, "vllm/vllm-openai:v0.27.1"),  // older rollout image than the template
		pod("stale", "rs-0", true, "vllm/vllm-openai:v0.27.1"),    // a deleted ReplicaSet with the same name
		pod("adopted", "rs-1", false, "vllm/vllm-openai:v0.27.1"), // owner reference without controller
	}
	r := Run(s, Options{})
	var got []string
	for _, w := range r.Workloads {
		var pods []string
		for _, t := range w.Timelines {
			pods = append(pods, t.Pod+"@"+t.Image)
		}
		got = append(got, w.Ref()+"="+strings.Join(pods, ","))
	}
	want := []string{
		"ai/deployment/llm=current@vllm/vllm-openai:v0.27.1",
		"ai/pod/adopted=adopted@vllm/vllm-openai:v0.27.1",
		"ai/replicaset/llm-abc=stale@vllm/vllm-openai:v0.27.1",
	}
	sort.Strings(got)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("workloads =\n%v\nwant\n%v", got, want)
	}
}

func TestAccessAndReliabilityFindings(t *testing.T) {
	one := int32(1)
	two := int32(2)
	s := &collect.Snapshot{Known: map[string]bool{
		"horizontalpodautoscalers.json": true, "scaledobjects.json": true, "poddisruptionbudgets.json": true,
	}}
	dep := kube.Workload{}
	dep.Metadata.Name, dep.Metadata.Namespace = "llm", "ai"
	dep.Spec.Replicas = &one
	dep.Spec.Template.Metadata.Labels = map[string]string{"app": "llm"}
	dep.Spec.Template.Spec.Containers = []kube.Container{{
		Name: "vllm", Image: "vllm/vllm-openai:latest",
		Args:          []string{"--model", "Qwen/Qwen2.5-7B-Instruct"},
		LivenessProbe: &kube.Probe{PeriodSeconds: 10, FailureThreshold: 3},
	}}
	dep.Kind = "Deployment"
	s.Workloads = []kube.Workload{dep}

	lb := kube.Service{}
	lb.Metadata.Name, lb.Metadata.Namespace = "llm-public", "ai"
	lb.Spec.Type, lb.Spec.Selector = "LoadBalancer", map[string]string{"app": "llm"}
	internal := lb
	internal.Metadata.Name = "llm-internal"
	internal.Metadata.Annotations = map[string]string{"service.beta.kubernetes.io/aws-load-balancer-internal": "true"}
	s.Services = []kube.Service{lb, internal}

	r := Run(s, Options{})
	want := []string{
		"access/image-latest", "access/internal-load-balancer", "access/load-balancer", "access/model-revision", "access/no-api-key",
		"reliability/liveness-during-load", "reliability/no-readiness-probe", "reliability/single-replica", "startup/model-cache-not-kept",
	}
	if got := ids(r.Findings); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("findings =\n%v\nwant\n%v", got, want)
	}

	// Two replicas behind a disruption budget, pinned, keyed and probed: nothing to report
	// except the tag note.
	dep.Spec.Replicas = &two
	c := &dep.Spec.Template.Spec.Containers[0]
	c.Image = "vllm/vllm-openai:v0.27.1"
	c.Args = []string{"--model", "/models/qwen", "--api-key", "$(KEY)"}
	c.LivenessProbe, c.ReadinessProbe = nil, &kube.Probe{}
	s.Workloads = []kube.Workload{dep}
	s.Services = nil
	pdb := kube.PodDisruptionBudget{}
	pdb.Metadata.Namespace = "ai"
	pdb.Spec.Selector = &kube.LabelSelector{MatchLabels: map[string]string{"app": "llm"}}
	s.PDBs = []kube.PodDisruptionBudget{pdb}
	r = Run(s, Options{})
	if got := ids(r.Findings); strings.Join(got, ",") != "access/image-tag" {
		t.Errorf("clean workload findings = %v", got)
	}

	// A budget that misses one of the pods is reported as partial, naming the pod.
	running := func(name string, labels map[string]string) *kube.Pod {
		p := &kube.Pod{}
		p.Metadata.Name, p.Metadata.Namespace, p.Metadata.Labels = name, "ai", labels
		return p
	}
	wl := &Workload{Kind: "Deployment", Namespace: "ai", Name: "llm", minReplica: 2,
		spec: dep.Spec.Template.Spec, labels: map[string]string{"app": "llm"},
		pods: []*kube.Pod{running("a", map[string]string{"app": "llm"}), running("b", map[string]string{"app": "llm-v2"})}}
	var partial []string
	for _, f := range reliabilityFindings(wl, s) {
		if f.ID == "reliability/partial-disruption-budget" {
			partial = append(partial, f.Evidence)
		}
	}
	if len(partial) != 1 || !strings.Contains(partial[0], "selects b.") {
		t.Errorf("partial budget: %v", partial)
	}

	// A PDB list that couldn't be read is no evidence that there's no PDB.
	s.PDBs, s.Known["poddisruptionbudgets.json"] = nil, false
	if got := ids(Run(s, Options{}).Findings); strings.Join(got, ",") != "access/image-tag" {
		t.Errorf("unread PDBs: findings = %v", got)
	}
	// Nor is an unreadable autoscaler list evidence of a fixed replica count.
	dep.Spec.Replicas = &one
	s.Workloads = []kube.Workload{dep}
	s.Known["horizontalpodautoscalers.json"] = false
	if got := ids(Run(s, Options{}).Findings); strings.Join(got, ",") != "access/image-tag" {
		t.Errorf("unread autoscalers: findings = %v", got)
	}

	// Settings from a shell script or envFrom are reported as unread, not as missing.
	c.Command, c.Args = []string{"sh", "-c", "vllm serve Qwen/Qwen2.5-7B-Instruct"}, nil
	s.Workloads = []kube.Workload{dep}
	if got := ids(Run(s, Options{}).Findings); strings.Join(got, ",") != "access/image-tag,access/vllm-settings-unread" {
		t.Errorf("shell command: findings = %v", got)
	}
}

func TestSelectAndGPUOnly(t *testing.T) {
	s := &collect.Snapshot{}
	gpu := kube.Pod{}
	gpu.Metadata.Name, gpu.Metadata.Namespace = "trainer", "ml"
	gpu.Spec.NodeName = "gpu-1"
	gpu.Spec.Containers = []kube.Container{{Name: "main", Image: "pytorch/pytorch",
		Resources: kube.Resources{Limits: map[string]kube.Quantity{"nvidia.com/gpu": "1"}}}}
	other := kube.Pod{}
	other.Metadata.Name, other.Metadata.Namespace = "api", "ml"
	other.Metadata.Labels = map[string]string{"serving": "yes"}
	other.Spec.Containers = []kube.Container{{Name: "api", Image: "example/api:1"}}
	s.Pods = []kube.Pod{gpu, other}
	node := kube.Node{}
	node.Metadata.Name = "gpu-1"
	node.Status.Allocatable = map[string]kube.Quantity{"nvidia.com/gpu": "4"}
	s.Nodes = []kube.Node{node}

	r := Run(s, Options{})
	if len(r.Workloads) != 1 || r.Workloads[0].Engine != "GPU workload" {
		t.Fatalf("without a selector: %+v", r.Workloads)
	}
	if r.Totals.GPUsAllocatable != 4 || r.Totals.GPUsRequested != 1 {
		t.Errorf("GPUs = %d of %d", r.Totals.GPUsRequested, r.Totals.GPUsAllocatable)
	}
	if got := ids(r.Findings); strings.Contains(strings.Join(got, ","), "gpus/") {
		t.Errorf("one namespace read can't show spare GPUs, got %v", got)
	}
	s.Meta.AllNamespaces = true
	if got := ids(Run(s, Options{}).Findings); !strings.Contains(strings.Join(got, ","), "gpus/unrequested") {
		t.Errorf("all namespaces read: findings = %v", got)
	}
	r = Run(s, Options{Selector: &kube.LabelSelector{MatchLabels: map[string]string{"serving": "yes"}}})
	if len(r.Workloads) != 2 {
		t.Errorf("with a selector: %d workloads", len(r.Workloads))
	}
}
