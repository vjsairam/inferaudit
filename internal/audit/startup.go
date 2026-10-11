package audit

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vjsairam/inferaudit/internal/kube"
)

// Timeline holds what pod status and kubelet events show about one pod's start. Pod status
// keeps only the latest Ready transition, so Ready can be later than the first time the pod
// served. Zero times are unknown.
type Timeline struct {
	Pod         string        `json:"pod"`
	Node        string        `json:"node,omitempty"`
	Container   string        `json:"container"`
	Image       string        `json:"image"`
	Created     time.Time     `json:"created"`
	PullStart   time.Time     `json:"pullStart,omitzero"`
	PullEnd     time.Time     `json:"pullEnd,omitzero"`
	PullTook    time.Duration `json:"pullTook,omitempty"`   // kubelet's figure, including time queued behind other pulls
	PullActive  time.Duration `json:"pullActive,omitempty"` // kubelet's figure without the queueing, when it reports both
	ImageBytes  int64         `json:"imageBytes,omitempty"`
	ImageCached bool          `json:"imageCached,omitempty"`
	Started     time.Time     `json:"started,omitzero"`
	Ready       time.Time     `json:"ready,omitzero"`
	IsReady     bool          `json:"isReady"`
	Restarts    int32         `json:"restarts,omitempty"`
	State       string        `json:"state"`
}

// Total is creation to Ready. It's 0 when unknown, and after a restart, when Ready no longer
// belongs to the first start.
func (t *Timeline) Total() time.Duration {
	if t.Restarts != 0 || t.Created.IsZero() || t.Ready.IsZero() || t.Ready.Before(t.Created) {
		return 0
	}
	return t.Ready.Sub(t.Created)
}

// StartToReady is container start to Ready, the window where most engines download, load
// and warm up the model. It's 0 when unknown or after a restart.
func (t *Timeline) StartToReady() time.Duration {
	if t.Restarts != 0 || t.Started.IsZero() || t.Ready.IsZero() || t.Ready.Before(t.Started) {
		return 0
	}
	return t.Ready.Sub(t.Started)
}

var (
	pulledRe  = regexp.MustCompile(`Successfully pulled image "([^"]+)" in ([0-9.]+[a-zµ0-9.]*)(?: \(([0-9.]+[a-zµ0-9.]*) including waiting\))?`)
	imageSize = regexp.MustCompile(`Image size: (\d+) bytes`)
	presentRe = regexp.MustCompile(`Container image "([^"]+)" already present on machine`)
	pullingRe = regexp.MustCompile(`Pulling image "([^"]+)"`)
)

// buildTimeline reads the pod's status and the kubelet's events for one container.
func buildTimeline(p *kube.Pod, c kube.Container, events []*kube.Event) *Timeline {
	t := &Timeline{Pod: p.Metadata.Name, Node: p.Spec.NodeName, Container: c.Name, Image: c.Image,
		Created: p.Metadata.CreationTimestamp.Time}
	if cond, ok := p.Condition("Ready"); ok {
		t.IsReady, t.Ready = true, cond.LastTransitionTime.Time
	}
	status := p.ContainerStatus(c.Name)
	if status != nil {
		t.Restarts = status.RestartCount
		if status.State.Running != nil {
			t.Started = status.State.Running.StartedAt.Time
		}
	}

	sort.Slice(events, func(i, j int) bool { return events[i].First().Before(events[j].First()) })
	for _, e := range events {
		if uid := e.InvolvedObject.UID; uid != "" && p.Metadata.UID != "" && uid != p.Metadata.UID {
			continue // an earlier pod with the same name
		}
		if !t.Created.IsZero() && e.When().Before(t.Created) {
			continue
		}
		if !aboutContainer(e, c, p) {
			continue
		}
		pulled := t.PullTook > 0 || t.ImageCached // only the first pull is kept
		switch e.Reason {
		case "Pulling":
			// Retries of one pull fold into a single event, so its first time is when the
			// pull began.
			if m := pullingRe.FindStringSubmatch(e.Message); m != nil && m[1] == c.Image && !pulled && t.PullStart.IsZero() {
				t.PullStart = e.First()
			}
		case "Pulled":
			if m := pulledRe.FindStringSubmatch(e.Message); m != nil && m[1] == c.Image && !pulled {
				t.PullEnd = e.When()
				t.PullTook = parseDur(m[2])
				if m[3] != "" {
					t.PullTook, t.PullActive = parseDur(m[3]), parseDur(m[2])
				}
				if s := imageSize.FindStringSubmatch(e.Message); s != nil {
					t.ImageBytes, _ = strconv.ParseInt(s[1], 10, 64)
				}
				if e.Count > 1 || e.Series != nil {
					// Several pulls folded together: keep the kubelet's figure but don't
					// draw an interval that can't be paired with its start.
					t.PullStart, t.PullEnd = time.Time{}, time.Time{}
				}
			} else if m := presentRe.FindStringSubmatch(e.Message); m != nil && m[1] == c.Image && !pulled {
				t.ImageCached, t.PullStart = true, time.Time{}
			}
		case "Started":
			if t.Started.IsZero() {
				t.Started = e.When()
			}
		}
	}

	switch {
	case t.IsReady:
		t.State = "Ready"
	case status != nil && status.State.Waiting != nil:
		t.State = status.State.Waiting.Reason
	case p.Status.Phase != "":
		t.State = p.Status.Phase
	default:
		t.State = "unknown"
	}
	return t
}

func parseDur(s string) time.Duration {
	d, _ := time.ParseDuration(strings.TrimRight(s, "."))
	return d
}

// aboutContainer keeps events about container c. Kubelet events name the container in their
// field path; without one, an event counts only if its message can't be about another container.
func aboutContainer(e *kube.Event, c kube.Container, p *kube.Pod) bool {
	if fp := e.InvolvedObject.FieldPath; fp != "" {
		return fp == "spec.containers{"+c.Name+"}"
	}
	if e.Reason == "Started" {
		return e.Message == "Started container "+c.Name || e.Message == "Started container: "+c.Name
	}
	if c.Image == "" || !strings.Contains(e.Message, `"`+c.Image+`"`) {
		return false
	}
	same := 0
	for _, other := range append(append([]kube.Container{}, p.Spec.Containers...), p.Spec.InitContainers...) {
		if other.Image == c.Image {
			same++
		}
	}
	return same == 1
}

// eventsByPod indexes pod events by namespace/name.
func eventsByPod(events []kube.Event) map[string][]*kube.Event {
	out := map[string][]*kube.Event{}
	for i := range events {
		e := &events[i]
		if e.InvolvedObject.Kind != "Pod" {
			continue
		}
		ns := e.InvolvedObject.Namespace
		if ns == "" {
			ns = e.Metadata.Namespace
		}
		key := ns + "/" + e.InvolvedObject.Name
		out[key] = append(out[key], e)
	}
	return out
}
