// Package audit turns a cluster snapshot into findings about how models are served.
// Findings describe what the configuration and timings show. Things they can't show,
// such as whether an endpoint is really reachable from the internet, are worded as checks.
package audit

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vjsairam/inferaudit/internal/collect"
	"github.com/vjsairam/inferaudit/internal/kube"
)

// Levels. A check is worth someone's time; a note is context.
const (
	Check = "check"
	Note  = "note"
)

// Areas, in report order.
var Areas = []string{"Startup", "Reliability", "Reproducibility and access", "GPUs"}

type Finding struct {
	ID       string `json:"id"`
	Area     string `json:"area"`
	Level    string `json:"level"`
	Workload string `json:"workload,omitempty"`
	Title    string `json:"title"`
	Evidence string `json:"evidence"`
	Why      string `json:"why"`
	Next     string `json:"next"`
}

type Workload struct {
	Kind       string      `json:"kind"`
	Namespace  string      `json:"namespace"`
	Name       string      `json:"name"`
	Engine     string      `json:"engine"`
	Container  string      `json:"container"`
	Image      string      `json:"image"`
	Replicas   string      `json:"replicas"`
	Autoscaler string      `json:"autoscaler,omitempty"`
	Timelines  []*Timeline `json:"timelines"`

	spec       kube.PodSpec
	labels     map[string]string
	replicas   *int32
	minReplica int32 // lowest replica count the workload can run at; -1 if unknown
	pods       []*kube.Pod
}

// Ref names the workload the way kubectl would: namespace/kind/name.
func (w *Workload) Ref() string {
	return w.Namespace + "/" + strings.ToLower(w.Kind) + "/" + w.Name
}

func (w *Workload) container() kube.Container {
	for _, c := range w.spec.Containers {
		if c.Name == w.Container {
			return c
		}
	}
	return kube.Container{}
}

type GPUNode struct {
	Name         string `json:"name"`
	InstanceType string `json:"instanceType,omitempty"`
	Allocatable  int    `json:"allocatable"`
	Requested    int    `json:"requested"`
}

type Totals struct {
	Workloads       int           `json:"workloads"`
	Pods            int           `json:"pods"`
	Checks          int           `json:"checks"`
	Notes           int           `json:"notes"`
	GPUsAllocatable int           `json:"gpusAllocatable"`
	GPUsRequested   int           `json:"gpusRequested"`
	SlowestStart    time.Duration `json:"slowestStart"`
	SlowestPod      string        `json:"slowestPod,omitempty"`
	SlowestPull     time.Duration `json:"slowestPull,omitempty"` // image pull within the slowest start
	GPUScope        string        `json:"gpuScope"`              // "cluster" or "namespace": whose GPU requests were counted
}

type Report struct {
	Version   string       `json:"version"`
	Meta      collect.Meta `json:"meta"`
	Totals    Totals       `json:"totals"`
	Workloads []*Workload  `json:"workloads"`
	Findings  []Finding    `json:"findings"`
	GPUNodes  []GPUNode    `json:"gpuNodes"`
}

// Options for an audit run.
type Options struct {
	Version  string
	Selector *kube.LabelSelector // extra workloads to include, matched on pod labels
}

// Run audits a snapshot.
func Run(s *collect.Snapshot, opts Options) *Report {
	r := &Report{Version: opts.Version, Meta: s.Meta}
	r.Workloads = findWorkloads(s, opts.Selector)
	events := eventsByPod(s.Events)

	for _, w := range r.Workloads {
		w.Replicas, w.Autoscaler, w.minReplica = describeScale(w, s)
		for _, p := range w.pods {
			t := buildTimeline(p, podContainer(p, w.Container), events[p.Metadata.Namespace+"/"+p.Metadata.Name])
			w.Timelines = append(w.Timelines, t)
			if d := t.Total(); d > r.Totals.SlowestStart {
				r.Totals.SlowestStart, r.Totals.SlowestPod, r.Totals.SlowestPull = d, w.Namespace+"/"+t.Pod, t.PullTook
			}
		}
		sort.Slice(w.Timelines, func(i, j int) bool { return w.Timelines[i].Created.Before(w.Timelines[j].Created) })
		r.Totals.Pods += len(w.pods)

		r.Findings = append(r.Findings, startupFindings(w)...)
		r.Findings = append(r.Findings, reliabilityFindings(w, s)...)
		r.Findings = append(r.Findings, accessFindings(w, s)...)
	}
	r.GPUNodes = gpuNodes(s)
	for _, n := range r.GPUNodes {
		r.Totals.GPUsAllocatable += n.Allocatable
		r.Totals.GPUsRequested += n.Requested
	}
	// Nodes are cluster-wide but pods may come from one namespace. Spare capacity is only
	// reported when every namespace's requests were counted.
	r.Totals.GPUScope = "namespace"
	if s.Meta.AllNamespaces {
		r.Totals.GPUScope = "cluster"
		r.Findings = append(r.Findings, gpuFindings(r.GPUNodes)...)
	}

	order := map[string]int{}
	for i, a := range Areas {
		order[a] = i
	}
	sort.SliceStable(r.Findings, func(i, j int) bool {
		a, b := r.Findings[i], r.Findings[j]
		if a.Level != b.Level {
			return a.Level == Check
		}
		if order[a.Area] != order[b.Area] {
			return order[a.Area] < order[b.Area]
		}
		return a.Workload < b.Workload
	})
	for _, f := range r.Findings {
		if f.Level == Check {
			r.Totals.Checks++
		} else {
			r.Totals.Notes++
		}
	}
	r.Totals.Workloads = len(r.Workloads)
	return r
}

// describeScale explains how many replicas the workload runs and what sets that number.
// min is the lowest count it can run at, or -1 when that can't be known from the snapshot.
func describeScale(w *Workload, s *collect.Snapshot) (replicas, autoscaler string, min int32) {
	switch w.Kind {
	case "Pod":
		return "1", "", 1
	case "Deployment", "StatefulSet":
	default:
		return "unknown", "", -1
	}
	min = -1
	if w.replicas != nil {
		min = *w.replicas
		replicas = fmt.Sprint(*w.replicas)
	}
	for _, so := range s.ScaledObjects {
		ref := so.Spec.ScaleTargetRef
		kind := ref.Kind
		if kind == "" {
			kind = "Deployment" // KEDA's default target kind
		}
		if so.Metadata.Namespace != w.Namespace || ref.Name != w.Name || kind != w.Kind {
			continue
		}
		lo, hi := int32(0), int32(100)
		if so.Spec.MinReplicaCount != nil {
			lo = *so.Spec.MinReplicaCount
		}
		if so.Spec.MaxReplicaCount != nil {
			hi = *so.Spec.MaxReplicaCount
		}
		if idle := so.Spec.IdleReplicaCount; idle != nil && *idle < lo {
			lo = *idle
		}
		return fmt.Sprintf("%d to %d", lo, hi), "KEDA ScaledObject " + so.Metadata.Name, lo
	}
	for _, h := range s.HPAs {
		ref := h.Spec.ScaleTargetRef
		if h.Metadata.Namespace != w.Namespace || ref.Name != w.Name || ref.Kind != w.Kind {
			continue
		}
		lo := int32(1)
		if h.Spec.MinReplicas != nil {
			lo = *h.Spec.MinReplicas
		}
		return fmt.Sprintf("%d to %d", lo, h.Spec.MaxReplicas), "HorizontalPodAutoscaler " + h.Metadata.Name, lo
	}
	// No autoscaler found. That only means there isn't one if both kinds were readable.
	if !s.Known["horizontalpodautoscalers.json"] || !s.Known["scaledobjects.json"] {
		return "unknown", "", -1
	}
	if replicas == "" {
		replicas = "unknown"
	}
	return replicas, "", min
}

func gpuNodes(s *collect.Snapshot) []GPUNode {
	requested := map[string]int{}
	for i := range s.Pods {
		p := &s.Pods[i]
		if terminated(p) || p.Spec.NodeName == "" {
			continue
		}
		requested[p.Spec.NodeName] += kube.PodGPUs(p.Spec)
	}
	var out []GPUNode
	for _, n := range s.Nodes {
		alloc := 0
		for _, name := range kube.GPUResources {
			alloc += kube.Count(n.Status.Allocatable[name])
		}
		if alloc == 0 {
			continue
		}
		out = append(out, GPUNode{
			Name:         n.Metadata.Name,
			InstanceType: n.Metadata.Labels["node.kubernetes.io/instance-type"],
			Allocatable:  alloc,
			Requested:    requested[n.Metadata.Name],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
