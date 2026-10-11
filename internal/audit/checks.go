package audit

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/vjsairam/inferaudit/internal/collect"
	"github.com/vjsairam/inferaudit/internal/kube"
)

// A pull or a load slower than these is long enough to matter when a new replica is meant
// to absorb extra traffic. A startup probe is compared with readiness when a measured start
// used this share of its budget.
const (
	slowPull        = 60 * time.Second
	slowStartToRdy  = 2 * time.Minute
	probeCloseShare = 0.75
)

func startupFindings(w *Workload) []Finding {
	var out []Finding
	var pull, load *Timeline
	for _, t := range w.Timelines {
		if t.PullTook > 0 && (pull == nil || t.PullTook > pull.PullTook) {
			pull = t
		}
		if t.StartToReady() > 0 && (load == nil || t.StartToReady() > load.StartToReady()) {
			load = t
		}
	}
	if pull != nil && pull.PullTook >= slowPull {
		detail := ""
		if pull.ImageBytes > 0 {
			detail = " (" + humanBytes(pull.ImageBytes) + ")"
		}
		queued := ""
		if pull.PullActive > 0 && pull.PullTook-pull.PullActive >= time.Second {
			queued = fmt.Sprintf(" %s of that was queued behind other pulls.", Dur(pull.PullTook-pull.PullActive))
		}
		out = append(out, Finding{
			ID: "startup/slow-image-pull", Area: "Startup", Level: Check, Workload: w.Ref(),
			Title:    "Image pull took " + Dur(pull.PullTook),
			Evidence: fmt.Sprintf("The kubelet reported pulling %s%s in %s for pod %s on node %s.%s", pull.Image, detail, Dur(pull.PullTook), pull.Pod, orUnknown(pull.Node), queued),
			Why:      "A new replica on a node without this image waits for the pull before the server can start. How long depends on the registry, the network and what the node already has cached.",
			Next:     "Time the same image from a registry in the cluster's region, and with the image already on the node, then compare.",
		})
	}
	if load != nil && load.StartToReady() >= slowStartToRdy {
		out = append(out, Finding{
			ID: "startup/slow-start-to-ready", Area: "Startup", Level: Check, Workload: w.Ref(),
			Title:    "Container start to latest Ready took " + Dur(load.StartToReady()),
			Evidence: fmt.Sprintf("Pod %s started its %s container at %s, and its Ready condition last turned true at %s.", load.Pod, load.Container, clock(load.Started), clock(load.Ready)),
			Why:      "Model download, loading and warmup usually happen in this window. Pod status keeps only the latest Ready transition, so if readiness dropped and came back since, the startup itself was shorter.",
			Next:     "Read the engine's startup log lines to see which part dominates. For vLLM, look for \"Model loading took\" and \"init engine\".",
		})
	}
	if f, ok := cacheFinding(w); ok {
		out = append(out, f)
	}
	return out
}

// cacheFinding flags vLLM pods whose Hugging Face cache doesn't outlive the pod, or notes
// that the cache location couldn't be worked out.
func cacheFinding(w *Workload) (Finding, bool) {
	if w.Engine != "vLLM" {
		return Finding{}, false
	}
	c := w.container()
	args := vllmArgs(c)
	if !args.known || len(c.EnvFrom) > 0 || !looksLikeHubID(args.model) {
		return Finding{}, false // reported once, by vllmFindings
	}
	dir, known := hfCacheDir(c, w.spec, args)
	if !known {
		return Finding{
			ID: "startup/model-cache-unknown", Area: "Startup", Level: Note, Workload: w.Ref(),
			Title:    "Couldn't tell where the model cache is",
			Evidence: fmt.Sprintf("vLLM serves %s, but its cache path couldn't be worked out from the pod spec and the image defaults.", args.model),
			Why:      "The check for whether downloaded weights outlive the pod was skipped.",
			Next:     "Find where the image keeps the Hugging Face cache and whether that path is on a volume that outlives the pod.",
		}, true
	}
	where := "isn't on a volume"
	if mount := mountFor(c, dir); mount != nil {
		where = ""
		for _, v := range w.spec.Volumes {
			if v.Name == mount.Name && len(v.EmptyDir) > 0 {
				where = "is on an emptyDir"
			}
		}
		if where == "" {
			return Finding{}, false // a volume that outlives the pod, or one this can't classify
		}
	}
	return Finding{
		ID: "startup/model-cache-not-kept", Area: "Startup", Level: Check, Workload: w.Ref(),
		Title:    "The model cache isn't kept between pods",
		Evidence: fmt.Sprintf("vLLM serves %s, and the Hugging Face cache at %s %s.", args.model, dir, where),
		Why:      "If the weights come from the Hub, every new replica downloads them again before it can load them, which adds to scale-up time.",
		Next:     "Confirm where the weights come from (the image, an init container or the Hub), then compare startup with a cache that outlives the pod.",
	}, true
}

func reliabilityFindings(w *Workload, s *collect.Snapshot) []Finding {
	var out []Finding
	c := w.container()

	const oneReplicaWhy = "Losing the only serving replica interrupts requests until a replacement is Ready, which can take as long as the startups above."
	switch {
	case w.Kind == "Pod":
		out = append(out, Finding{
			ID: "reliability/no-controller", Area: "Reliability", Level: Check, Workload: w.Ref(),
			Title:    "Pod has no controller",
			Evidence: "No owner reference on this pod is marked as its controller.",
			Why:      "Unless something outside Kubernetes manages it, nothing recreates the pod if it or its node goes away.",
			Next:     "Confirm how it gets recreated, and run it under a Deployment or StatefulSet if nothing does.",
		})
	case w.minReplica == 1 && w.Autoscaler != "":
		out = append(out, Finding{
			ID: "reliability/single-replica", Area: "Reliability", Level: Check, Workload: w.Ref(),
			Title:    "Can drop to a single replica",
			Evidence: fmt.Sprintf("%s is set to %s replicas.", w.Autoscaler, w.Replicas),
			Why:      oneReplicaWhy,
			Next:     "If this model needs to stay up, keep a second replica or a fallback route.",
		})
	case w.minReplica == 1:
		out = append(out, Finding{
			ID: "reliability/single-replica", Area: "Reliability", Level: Check, Workload: w.Ref(),
			Title:    "Runs as a single replica",
			Evidence: fmt.Sprintf("The %s is set to 1 replica, and no autoscaler in the snapshot targets it.", w.Kind),
			Why:      oneReplicaWhy,
			Next:     "If this model needs to stay up, run a second replica or put a fallback route in front.",
		})
	}

	if w.minReplica >= 2 && s.Known["poddisruptionbudgets.json"] {
		switch uncovered, total := uncoveredByPDB(w, s); {
		case len(uncovered) == total:
			out = append(out, Finding{
				ID: "reliability/no-disruption-budget", Area: "Reliability", Level: Note, Workload: w.Ref(),
				Title:    "No PodDisruptionBudget",
				Evidence: "No PodDisruptionBudget in the namespace selects these pods.",
				Why:      "A node drain or cluster upgrade can evict several replicas at once.",
				Next:     "Add a PodDisruptionBudget that keeps at least one replica serving.",
			})
		case len(uncovered) > 0:
			out = append(out, Finding{
				ID: "reliability/partial-disruption-budget", Area: "Reliability", Level: Note, Workload: w.Ref(),
				Title:    "PodDisruptionBudget doesn't cover every pod",
				Evidence: "No PodDisruptionBudget selects " + strings.Join(uncovered, ", ") + ".",
				Why:      "Those pods can be evicted without the budget holding anything back, which often means a rollout changed their labels.",
				Next:     "Check the budget's selector against the labels on those pods.",
			})
		}
	}

	if c.ReadinessProbe == nil {
		out = append(out, Finding{
			ID: "reliability/no-readiness-probe", Area: "Reliability", Level: Check, Workload: w.Ref(),
			Title:    "No readiness probe on the serving container",
			Evidence: fmt.Sprintf("Container %s has no readinessProbe.", c.Name),
			Why:      "Without its own readiness check, the pod can be marked Ready before the model has loaded, so its Service may send requests the server can't answer yet.",
			Next:     "Add a readiness probe on the engine's health endpoint.",
		})
	}

	var load time.Duration
	for _, t := range w.Timelines {
		load = max(load, t.StartToReady())
	}
	switch {
	case c.StartupProbe == nil && c.LivenessProbe != nil:
		budget := c.LivenessProbe.Budget()
		evidence := fmt.Sprintf("The liveness probe's settings give it about %s and there's no startup probe.", Dur(budget))
		if load > budget {
			evidence += fmt.Sprintf(" A pod here took %s from container start to its latest Ready transition, which can include readiness recovering later on.", Dur(load))
		}
		out = append(out, Finding{
			ID: "reliability/liveness-during-load", Area: "Reliability", Level: Note, Workload: w.Ref(),
			Title:    "Check liveness behaviour during startup",
			Evidence: evidence,
			Why:      "If the liveness endpoint fails while the model loads, the kubelet restarts the container and loading starts over. These objects don't show whether it does.",
			Next:     "Check what the liveness endpoint returns during a cold start. Add a startup probe if it can fail while loading.",
		})
	case c.StartupProbe != nil && load > 0 && float64(load) >= probeCloseShare*float64(c.StartupProbe.Budget()):
		out = append(out, Finding{
			ID: "reliability/startup-probe-close", Area: "Reliability", Level: Note, Workload: w.Ref(),
			Title:    "Compare the startup probe with startup logs",
			Evidence: fmt.Sprintf("The startup probe's settings give it about %s, and a pod here took %s from container start to its latest Ready transition, which can include readiness recovering later on.", Dur(c.StartupProbe.Budget()), Dur(load)),
			Why:      "If the startup probe is what holds the pod back, a slower download or a bigger model can push it past its limit, and the container restarts mid-load. Readiness can also lag for other reasons.",
			Next:     "Find when the startup probe first passed. Raise its failureThreshold only if it was close to running out.",
		})
	}
	return out
}

// uncoveredByPDB lists the observed pods no PodDisruptionBudget in the namespace selects,
// out of how many there are. With no pods running, the template stands in for them.
func uncoveredByPDB(w *Workload, s *collect.Snapshot) (uncovered []string, total int) {
	matches := func(labels map[string]string) bool {
		for _, pdb := range s.PDBs {
			if pdb.Metadata.Namespace == w.Namespace && pdb.Spec.Selector.Matches(labels) {
				return true
			}
		}
		return false
	}
	if len(w.pods) == 0 {
		if !matches(w.labels) {
			return []string{"the pod template"}, 1
		}
		return nil, 1
	}
	for _, p := range w.pods {
		if !matches(p.Metadata.Labels) {
			uncovered = append(uncovered, p.Metadata.Name)
		}
	}
	return uncovered, len(w.pods)
}

func accessFindings(w *Workload, s *collect.Snapshot) []Finding {
	var out []Finding
	c := w.container()

	if !strings.Contains(c.Image, "@") {
		tag := imageTag(c.Image)
		if tag == "" || tag == "latest" {
			out = append(out, Finding{
				ID: "access/image-latest", Area: "Reproducibility and access", Level: Check, Workload: w.Ref(),
				Title:    "Image has no fixed version",
				Evidence: fmt.Sprintf("Container %s runs %s.", c.Name, c.Image),
				Why:      "A new pod can start a different engine version from the pods already running, with no change in the manifest.",
				Next:     "Pin the image by digest, for example image@sha256:...",
			})
		} else {
			out = append(out, Finding{
				ID: "access/image-tag", Area: "Reproducibility and access", Level: Note, Workload: w.Ref(),
				Title:    "Image uses a tag",
				Evidence: fmt.Sprintf("Container %s runs %s.", c.Name, c.Image),
				Why:      "Tags can be moved to a new build; a digest can't.",
				Next:     "Pin the image by digest if you need every replica to run the same build.",
			})
		}
	}

	if w.Engine == "vLLM" {
		out = append(out, vllmFindings(w, c)...)
	}

	for _, svc := range s.Services {
		if svc.Metadata.Namespace != w.Namespace || len(svc.Spec.Selector) == 0 || !serviceSelects(w, svc.Spec.Selector) {
			continue
		}
		switch svc.Spec.Type {
		case "LoadBalancer":
			addr := "no address assigned yet"
			if ing := svc.Status.LoadBalancer.Ingress; len(ing) > 0 {
				addr = "address " + ing[0].IP + ing[0].Hostname
			}
			if internalLB(svc.Metadata.Annotations) {
				out = append(out, Finding{
					ID: "access/internal-load-balancer", Area: "Reproducibility and access", Level: Note, Workload: w.Ref(),
					Title:    "An internal LoadBalancer Service selects these pods",
					Evidence: fmt.Sprintf("Service %s is a LoadBalancer annotated as internal (%s).", svc.Metadata.Name, addr),
					Why:      "The annotation asks the cloud for a private load balancer; routing and access rules aren't visible here.",
					Next:     "Check which networks can reach it and that callers authenticate.",
				})
				continue
			}
			out = append(out, Finding{
				ID: "access/load-balancer", Area: "Reproducibility and access", Level: Check, Workload: w.Ref(),
				Title:    "A LoadBalancer Service selects these pods",
				Evidence: fmt.Sprintf("Service %s is type LoadBalancer with no internal-only annotation this report recognises (%s).", svc.Metadata.Name, addr),
				Why:      "On most clouds that gives the pods an address outside the cluster, often a public one. The port it forwards may be the model API or another container's.",
				Next:     "Confirm the target port, whether the address is reachable from the internet, and what authenticates requests.",
			})
		case "NodePort":
			out = append(out, Finding{
				ID: "access/node-port", Area: "Reproducibility and access", Level: Note, Workload: w.Ref(),
				Title:    "A NodePort Service selects these pods",
				Evidence: fmt.Sprintf("Service %s is type NodePort.", svc.Metadata.Name),
				Why:      "NodePort opens the selected port on node addresses. Whether that's reachable depends on node networking and firewalls.",
				Next:     "Confirm the target port and which networks can reach the nodes on it.",
			})
		}
	}
	return out
}

func vllmFindings(w *Workload, c kube.Container) []Finding {
	args := vllmArgs(c)
	if !args.known || len(c.EnvFrom) > 0 {
		evidence := fmt.Sprintf("Container %s doesn't start vLLM in a way this report reads: the command isn't vllm, python -m vllm.entrypoints or the official image's own entrypoint, or it passes a --config file.", c.Name)
		if args.known {
			evidence = fmt.Sprintf("Container %s loads environment variables with envFrom, which this report can't see into.", c.Name)
		}
		return []Finding{{
			ID: "access/vllm-settings-unread", Area: "Reproducibility and access", Level: Note, Workload: w.Ref(),
			Title:    "Some vLLM settings couldn't be read",
			Evidence: evidence,
			Why:      "The checks for model revision, API key and model cache were skipped rather than guessed.",
			Next:     "Check those three settings by hand.",
		}}
	}
	var out []Finding
	if looksLikeHubID(args.model) && !args.has("--revision") {
		out = append(out, Finding{
			ID: "access/model-revision", Area: "Reproducibility and access", Level: Check, Workload: w.Ref(),
			Title:    "Model revision isn't pinned",
			Evidence: fmt.Sprintf("vLLM serves %s with no --revision.", args.model),
			Why:      "If the weights come from the Hub, a new pod downloads whatever is on the default branch at the time, so replicas can end up serving different weights after the repository changes.",
			Next:     "Pass --revision with a commit hash from the model repository.",
		})
	}
	if _, set := c.EnvValue("VLLM_API_KEY"); !set && !args.has("--api-key") {
		out = append(out, Finding{
			ID: "access/no-api-key", Area: "Reproducibility and access", Level: Check, Workload: w.Ref(),
			Title:    "No vLLM API key setting found",
			Evidence: "Neither --api-key nor VLLM_API_KEY is set on the serving container.",
			Why:      "vLLM then answers anyone who can reach it. That's fine behind a gateway that authenticates requests, but these objects don't show one.",
			Next:     "Confirm what authenticates requests before they reach this Service.",
		})
	}
	return out
}

func gpuFindings(nodes []GPUNode) []Finding {
	idle, total := 0, 0
	var names []string
	for _, n := range nodes {
		total += n.Allocatable
		if free := n.Allocatable - n.Requested; free > 0 {
			idle += free
			names = append(names, n.Name)
		}
	}
	if idle == 0 {
		return nil
	}
	return []Finding{{
		ID: "gpus/unrequested", Area: "GPUs", Level: Note,
		Title:    fmt.Sprintf("%d of %d GPUs have no pod requesting them", idle, total),
		Evidence: "Nodes with GPUs no running pod requests: " + strings.Join(names, ", ") + ".",
		Why:      "A GPU no pod requests is paid for while nothing runs on it. These are nvidia.com/gpu and amd.com/gpu counts, which can be time-sliced shares rather than whole GPUs, and requests aren't utilisation.",
		Next:     "Check whether these nodes are spare capacity on purpose, waiting for a scale-up, or left over.",
	}}
}

// internalLB recognises the common cloud annotations that keep a load balancer private.
func internalLB(a map[string]string) bool {
	switch {
	case a["service.beta.kubernetes.io/aws-load-balancer-internal"] == "true",
		a["service.beta.kubernetes.io/aws-load-balancer-scheme"] == "internal",
		strings.EqualFold(a["networking.gke.io/load-balancer-type"], "Internal"),
		strings.EqualFold(a["cloud.google.com/load-balancer-type"], "Internal"),
		a["service.beta.kubernetes.io/azure-load-balancer-internal"] == "true",
		a["service.beta.kubernetes.io/oci-load-balancer-internal"] == "true":
		return true
	}
	return false
}

// serviceSelects reports whether a Service selector matches the workload's pods, or its
// template when no pods are running.
func serviceSelects(w *Workload, selector map[string]string) bool {
	if len(w.pods) == 0 {
		return subset(selector, w.labels)
	}
	for _, p := range w.pods {
		if subset(selector, p.Metadata.Labels) {
			return true
		}
	}
	return false
}

func subset(sel, labels map[string]string) bool {
	for k, v := range sel {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// imageTag returns the tag of an image reference without a digest, or "" if it has none.
func imageTag(image string) string {
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if _, tag, ok := strings.Cut(name, ":"); ok {
		return tag
	}
	return ""
}

type engineArgs struct {
	model string
	flags map[string]string
	known bool // false when settings may come from a shell script or config file
}

func (a engineArgs) has(flag string) bool { return a.flags[flag] != "" }

// vllmArgs reads the model and the few flags the checks need from a vLLM command line, in
// either the `vllm serve MODEL` or the `--model MODEL` form. Only `vllm`, `python -m
// vllm.entrypoints...` and the official image's own entrypoint are parsed. Anything else, such
// as a shell, a start script, a custom image's entrypoint or a --config file, leaves the
// settings unknown.
func vllmArgs(c kube.Container) engineArgs {
	a := engineArgs{flags: map[string]string{}, known: true}
	words := append(append([]string{}, c.Command...), c.Args...)
	var direct bool
	if len(c.Command) == 0 {
		direct = officialVLLM(c.Image)
	} else {
		base := path.Base(c.Command[0])
		direct = base == "vllm" ||
			(strings.HasPrefix(base, "python") && len(words) > 2 && words[1] == "-m" && strings.HasPrefix(words[2], "vllm.entrypoints."))
	}
	if !direct {
		a.known = false
		return a
	}
	for i := 0; i < len(words); i++ {
		word := words[i]
		if word == "serve" && (i == 0 || path.Base(words[i-1]) == "vllm") && i+1 < len(words) && !strings.HasPrefix(words[i+1], "-") {
			a.model = words[i+1]
			i++
			continue
		}
		name, value, hasValue := strings.Cut(word, "=")
		switch name {
		case "--model", "--revision", "--api-key", "--download-dir", "--config":
			if !hasValue && i+1 < len(words) && !strings.HasPrefix(words[i+1], "-") {
				i++
				value = words[i]
			}
			a.flags[name] = value
			if name == "--config" {
				a.known = false
			}
		}
	}
	if a.model == "" {
		a.model = a.flags["--model"]
	}
	return a
}

// looksLikeHubID is true for org/name model IDs, as opposed to local paths or object storage.
func looksLikeHubID(model string) bool {
	return strings.Count(model, "/") == 1 && !strings.HasPrefix(model, "/") && !strings.HasPrefix(model, ".") &&
		!strings.Contains(model, "://") && !strings.Contains(model, "$")
}

// hfCacheDir works out where vLLM keeps downloaded weights. It answers only when the location
// is spelled out in the pod, or follows from the official image's defaults.
func hfCacheDir(c kube.Container, spec kube.PodSpec, a engineArgs) (string, bool) {
	literal := func(v string) (string, bool) {
		if !path.IsAbs(v) || strings.Contains(v, "$") {
			return "", false
		}
		return path.Clean(v), true
	}
	if v, ok := a.flags["--download-dir"]; ok {
		return literal(v)
	}
	for _, name := range []string{"HF_HUB_CACHE", "HUGGINGFACE_HUB_CACHE"} {
		if v, set := c.EnvValue(name); set {
			return literal(v)
		}
	}
	if v, set := c.EnvValue("HF_HOME"); set {
		root, ok := literal(v)
		return path.Join(root, "hub"), ok
	}
	if v, set := c.EnvValue("XDG_CACHE_HOME"); set {
		root, ok := literal(v)
		return path.Join(root, "huggingface", "hub"), ok
	}
	if v, set := c.EnvValue("HOME"); set {
		root, ok := literal(v)
		return path.Join(root, ".cache", "huggingface", "hub"), ok
	}
	// The vllm/vllm-openai image runs as root, so its default cache is under /root, unless
	// the pod picks another user.
	if officialVLLM(c.Image) && !nonRoot(c, spec) {
		return "/root/.cache/huggingface/hub", true
	}
	return "", false
}

func officialVLLM(image string) bool {
	repo := imageRepo(image)
	return repo == "vllm/vllm-openai" || repo == "docker.io/vllm/vllm-openai"
}

// imageRepo strips the tag and digest from an image reference.
func imageRepo(image string) string {
	repo, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return repo
}

func nonRoot(c kube.Container, spec kube.PodSpec) bool {
	if sc := c.SecurityContext; sc != nil && sc.RunAsUser != nil {
		return *sc.RunAsUser != 0
	}
	if sc := spec.SecurityContext; sc != nil && sc.RunAsUser != nil {
		return *sc.RunAsUser != 0
	}
	return false
}

// mountFor returns the volume mount that holds p, preferring the deepest match.
func mountFor(c kube.Container, p string) *kube.VolumeMount {
	var best *kube.VolumeMount
	for i, m := range c.VolumeMounts {
		mp := strings.TrimRight(m.MountPath, "/")
		if p == mp || strings.HasPrefix(p, mp+"/") {
			if best == nil || len(mp) > len(strings.TrimRight(best.MountPath, "/")) {
				best = &c.VolumeMounts[i]
			}
		}
	}
	return best
}

// Dur formats a duration the way people say it: 4m 38s, 52s, 1h 05m.
func Dur(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

func clock(t time.Time) string { return t.UTC().Format("15:04:05") + " UTC" }

func humanBytes(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.0f MB", float64(n)/1e6)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
