package audit

import (
	"sort"
	"strings"

	"github.com/vjsairam/inferaudit/internal/collect"
	"github.com/vjsairam/inferaudit/internal/kube"
)

// engineOf names the serving engine a container runs, or returns "" if it doesn't look like
// model serving. Containers that only ask for GPUs are reported as "GPU workload".
func engineOf(c kube.Container) string {
	img := strings.ToLower(c.Image)
	cmd := strings.ToLower(strings.Join(append(append([]string{}, c.Command...), c.Args...), " "))
	switch {
	case strings.Contains(img, "vllm"), strings.Contains(cmd, "vllm serve"), strings.Contains(cmd, "vllm.entrypoints"):
		return "vLLM"
	case strings.Contains(img, "text-generation-inference"):
		return "TGI"
	case strings.Contains(img, "tritonserver"):
		return "Triton"
	case strings.Contains(img, "sglang"):
		return "SGLang"
	case strings.Contains(img, "ollama"):
		return "Ollama"
	case strings.HasPrefix(img, "nvcr.io/nim/"):
		return "NIM"
	case strings.Contains(img, "kserve/huggingfaceserver"):
		return "KServe Hugging Face"
	}
	if c.GPUs() > 0 {
		return "GPU workload"
	}
	return ""
}

// servingContainer picks the container that serves the model: a known engine first, then any
// container asking for GPUs. With a selector match and neither, the first container is used.
func servingContainer(spec kube.PodSpec, selected bool) (kube.Container, string, bool) {
	var gpu *kube.Container
	for i, c := range spec.Containers {
		switch e := engineOf(c); e {
		case "":
		case "GPU workload":
			if gpu == nil {
				gpu = &spec.Containers[i]
			}
		default:
			return c, e, true
		}
	}
	if gpu != nil {
		return *gpu, "GPU workload", true
	}
	if kube.PodGPUs(spec) > 0 && len(spec.Containers) > 0 { // GPUs held by an init container
		return spec.Containers[0], "GPU workload", true
	}
	if selected && len(spec.Containers) > 0 {
		return spec.Containers[0], "selected by label", true
	}
	return kube.Container{}, "", false
}

// podContainer finds the serving container in a pod's own spec, which can differ from the
// current template during a rollout. The template's container name is only a fallback.
func podContainer(p *kube.Pod, name string) kube.Container {
	if c, _, ok := servingContainer(p.Spec, false); ok {
		return c
	}
	for _, c := range p.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	c, _, _ := servingContainer(p.Spec, true)
	return c
}

func terminated(p *kube.Pod) bool {
	return p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed"
}

func workloadKey(kind, namespace, name, uid string) string {
	return kind + "/" + namespace + "/" + name + "/" + uid
}

// findWorkloads groups serving pods under the Deployment or StatefulSet that controls them,
// following controller owner references by UID. Serving pods without such a controller are
// reported on their own.
func findWorkloads(s *collect.Snapshot, sel *kube.LabelSelector) []*Workload {
	replicaSets := map[string]*kube.ReplicaSet{}
	for i := range s.ReplicaSets {
		if uid := s.ReplicaSets[i].Metadata.UID; uid != "" {
			replicaSets[uid] = &s.ReplicaSets[i]
		}
	}

	byKey := map[string]*Workload{}
	var out []*Workload
	for i := range s.Workloads {
		w := &s.Workloads[i]
		selected := sel != nil && sel.Matches(w.Spec.Template.Metadata.Labels)
		c, engine, ok := servingContainer(w.Spec.Template.Spec, selected)
		if !ok {
			continue
		}
		wl := &Workload{
			Kind: w.Kind, Namespace: w.Metadata.Namespace, Name: w.Metadata.Name,
			Engine: engine, Container: c.Name, Image: c.Image,
			spec: w.Spec.Template.Spec, labels: w.Spec.Template.Metadata.Labels, replicas: w.Spec.Replicas,
		}
		byKey[workloadKey(w.Kind, wl.Namespace, wl.Name, w.Metadata.UID)] = wl
		out = append(out, wl)
	}

	for i := range s.Pods {
		p := &s.Pods[i]
		if terminated(p) {
			continue
		}
		ns := p.Metadata.Namespace
		kind, name, uid := "Pod", p.Metadata.Name, p.Metadata.UID
		if owner, ok := p.Metadata.ControllerOwner(); ok {
			kind, name, uid = owner.Kind, owner.Name, owner.UID
			if rs, ok := replicaSets[owner.UID]; ok && owner.Kind == "ReplicaSet" && rs.Metadata.Name == owner.Name {
				if d, ok := rs.Metadata.ControllerOwner(); ok && d.Kind == "Deployment" {
					kind, name, uid = d.Kind, d.Name, d.UID
				}
			}
		}
		if uid == "" {
			uid = "pod:" + p.Metadata.Name // ownership can't be confirmed, so don't merge pods
		}
		key := workloadKey(kind, ns, name, uid)
		if wl, ok := byKey[key]; ok {
			wl.pods = append(wl.pods, p)
			continue
		}
		selected := sel != nil && sel.Matches(p.Metadata.Labels)
		c, engine, ok := servingContainer(p.Spec, selected)
		if !ok {
			continue
		}
		wl := &Workload{Kind: kind, Namespace: ns, Name: name, Engine: engine,
			Container: c.Name, Image: c.Image, spec: p.Spec, labels: p.Metadata.Labels}
		byKey[key] = wl
		out = append(out, wl)
		wl.pods = append(wl.pods, p)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}
