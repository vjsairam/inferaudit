// Package kube holds the subset of Kubernetes object fields inferaudit reads.
// Objects are decoded from `kubectl get -o json` output, so field names follow the API.
package kube

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Time accepts RFC 3339 timestamps with or without fractions, and treats null or "" as unset.
type Time struct{ time.Time }

func (t *Time) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.Time.UTC().Format(time.RFC3339Nano))
}

type List[T any] struct {
	Items []T `json:"items"`
}

type OwnerReference struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

type ObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp Time              `json:"creationTimestamp"`
	OwnerReferences   []OwnerReference  `json:"ownerReferences"`
}

// ControllerOwner returns the owner reference marked as the object's controller.
func (m ObjectMeta) ControllerOwner() (OwnerReference, bool) {
	for _, o := range m.OwnerReferences {
		if o.Controller {
			return o, true
		}
	}
	return OwnerReference{}, false
}

type LabelSelectorRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
}

type LabelSelector struct {
	MatchLabels      map[string]string          `json:"matchLabels"`
	MatchExpressions []LabelSelectorRequirement `json:"matchExpressions"`
}

// Matches reports whether labels satisfy the selector. An empty selector matches everything.
func (s *LabelSelector) Matches(labels map[string]string) bool {
	if s == nil {
		return false
	}
	for k, v := range s.MatchLabels {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	for _, req := range s.MatchExpressions {
		got, ok := labels[req.Key]
		switch req.Operator {
		case "In":
			if !ok || !contains(req.Values, got) {
				return false
			}
		case "NotIn":
			if ok && contains(req.Values, got) {
				return false
			}
		case "Exists":
			if !ok {
				return false
			}
		case "DoesNotExist":
			if ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

type EnvVar struct {
	Name      string          `json:"name"`
	Value     string          `json:"value"`
	ValueFrom json.RawMessage `json:"valueFrom"`
}

// Quantity is a resource quantity. The API serialises them as strings, but hand-written
// manifests often use bare numbers, so both are accepted.
type Quantity string

func (q *Quantity) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*q = Quantity(s)
		return nil
	}
	*q = Quantity(b)
	return nil
}

type Resources struct {
	Requests map[string]Quantity `json:"requests"`
	Limits   map[string]Quantity `json:"limits"`
}

type Probe struct {
	InitialDelaySeconds int32 `json:"initialDelaySeconds"`
	PeriodSeconds       int32 `json:"periodSeconds"`
	FailureThreshold    int32 `json:"failureThreshold"`
}

// Budget is the nominal time a probe that never passes gets before the kubelet acts:
// the initial delay plus period times failure threshold, with the API defaults of 10s and 3.
// Probe timeouts can stretch it in practice.
func (p *Probe) Budget() time.Duration {
	period, failures := int64(p.PeriodSeconds), int64(p.FailureThreshold)
	if period == 0 {
		period = 10
	}
	if failures == 0 {
		failures = 3
	}
	return time.Duration(int64(p.InitialDelaySeconds)+period*failures) * time.Second
}

type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type SecurityContext struct {
	RunAsUser *int64 `json:"runAsUser"`
}

type Container struct {
	Name            string            `json:"name"`
	Image           string            `json:"image"`
	Command         []string          `json:"command"`
	Args            []string          `json:"args"`
	Env             []EnvVar          `json:"env"`
	EnvFrom         []json.RawMessage `json:"envFrom"`
	Resources       Resources         `json:"resources"`
	VolumeMounts    []VolumeMount     `json:"volumeMounts"`
	LivenessProbe   *Probe            `json:"livenessProbe"`
	ReadinessProbe  *Probe            `json:"readinessProbe"`
	StartupProbe    *Probe            `json:"startupProbe"`
	RestartPolicy   string            `json:"restartPolicy"` // "Always" marks a sidecar init container
	SecurityContext *SecurityContext  `json:"securityContext"`
}

// EnvValue returns an environment variable's literal value and whether it is set to something:
// a non-empty value, or a value taken from a Secret or ConfigMap (which reads back as "").
func (c *Container) EnvValue(name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, e.Value != "" || len(e.ValueFrom) > 0
		}
	}
	return "", false
}

type Volume struct {
	Name                  string          `json:"name"`
	EmptyDir              json.RawMessage `json:"emptyDir"`
	PersistentVolumeClaim json.RawMessage `json:"persistentVolumeClaim"`
	HostPath              json.RawMessage `json:"hostPath"`
}

type PodSpec struct {
	NodeName        string              `json:"nodeName"`
	Containers      []Container         `json:"containers"`
	InitContainers  []Container         `json:"initContainers"`
	Volumes         []Volume            `json:"volumes"`
	Overhead        map[string]Quantity `json:"overhead"`
	SecurityContext *SecurityContext    `json:"securityContext"`
}

type PodCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	LastTransitionTime Time   `json:"lastTransitionTime"`
}

type ContainerState struct {
	Running *struct {
		StartedAt Time `json:"startedAt"`
	} `json:"running"`
	Waiting *struct {
		Reason string `json:"reason"`
	} `json:"waiting"`
	Terminated *struct {
		Reason string `json:"reason"`
	} `json:"terminated"`
}

type ContainerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int32          `json:"restartCount"`
	State        ContainerState `json:"state"`
}

type PodStatus struct {
	Phase             string            `json:"phase"`
	Conditions        []PodCondition    `json:"conditions"`
	ContainerStatuses []ContainerStatus `json:"containerStatuses"`
}

type Pod struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     PodSpec    `json:"spec"`
	Status   PodStatus  `json:"status"`
}

// Condition returns the named condition if it is currently True.
func (p *Pod) Condition(kind string) (PodCondition, bool) {
	for _, c := range p.Status.Conditions {
		if c.Type == kind && c.Status == "True" {
			return c, true
		}
	}
	return PodCondition{}, false
}

func (p *Pod) ContainerStatus(name string) *ContainerStatus {
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == name {
			return &p.Status.ContainerStatuses[i]
		}
	}
	return nil
}

type PodTemplate struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     PodSpec    `json:"spec"`
}

// Workload covers Deployments and StatefulSets; both carry the fields read here.
type Workload struct {
	Kind     string     `json:"kind"`
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Replicas *int32        `json:"replicas"`
		Selector LabelSelector `json:"selector"`
		Template PodTemplate   `json:"template"`
	} `json:"spec"`
}

type ReplicaSet struct {
	Metadata ObjectMeta `json:"metadata"`
}

type Service struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Type     string            `json:"type"`
		Selector map[string]string `json:"selector"`
	} `json:"spec"`
	Status struct {
		LoadBalancer struct {
			Ingress []struct {
				IP       string `json:"ip"`
				Hostname string `json:"hostname"`
			} `json:"ingress"`
		} `json:"loadBalancer"`
	} `json:"status"`
}

type PodDisruptionBudget struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Selector *LabelSelector `json:"selector"`
	} `json:"spec"`
}

type ScaleTarget struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type HorizontalPodAutoscaler struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		ScaleTargetRef ScaleTarget `json:"scaleTargetRef"`
		MinReplicas    *int32      `json:"minReplicas"`
		MaxReplicas    int32       `json:"maxReplicas"`
	} `json:"spec"`
}

type ScaledObject struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		ScaleTargetRef   ScaleTarget `json:"scaleTargetRef"`
		MinReplicaCount  *int32      `json:"minReplicaCount"`
		IdleReplicaCount *int32      `json:"idleReplicaCount"`
		MaxReplicaCount  *int32      `json:"maxReplicaCount"`
		CooldownPeriod   *int32      `json:"cooldownPeriod"`
		Triggers         []struct {
			Type string `json:"type"`
		} `json:"triggers"`
	} `json:"spec"`
}

type Event struct {
	Metadata       ObjectMeta `json:"metadata"`
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		UID       string `json:"uid"`
		FieldPath string `json:"fieldPath"`
	} `json:"involvedObject"`
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	Count          int32  `json:"count"`
	FirstTimestamp Time   `json:"firstTimestamp"`
	LastTimestamp  Time   `json:"lastTimestamp"`
	EventTime      Time   `json:"eventTime"`
	Series         *struct {
		LastObservedTime Time `json:"lastObservedTime"`
	} `json:"series"`
}

// First returns when the event first happened.
func (e *Event) First() time.Time {
	if !e.FirstTimestamp.IsZero() {
		return e.FirstTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.LastTimestamp.Time
}

// When returns when the event last happened.
func (e *Event) When() time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.FirstTimestamp.Time
	}
}

type Node struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   struct {
		Allocatable map[string]Quantity `json:"allocatable"`
	} `json:"status"`
}

// GPUResources are the extended resource names counted as GPUs.
var GPUResources = []string{"nvidia.com/gpu", "amd.com/gpu"}

// Count parses a whole-number quantity such as an extended resource count. Anything else is 0.
func Count(q Quantity) int {
	n, err := strconv.Atoi(strings.TrimSpace(string(q)))
	if err != nil {
		return 0
	}
	return n
}

// Request returns what the container asks for of an extended resource. For those, the
// request defaults to the limit when only a limit is set.
func (c *Container) Request(resource string) int {
	if q, ok := c.Resources.Requests[resource]; ok {
		return Count(q)
	}
	return Count(c.Resources.Limits[resource])
}

// GPUs returns the GPUs a container asks for.
func (c *Container) GPUs() int {
	total := 0
	for _, name := range GPUResources {
		total += c.Request(name)
	}
	return total
}

// PodGPUs returns the GPUs a pod holds on its node, worked out the way the scheduler does:
// app containers plus sidecars, or the largest init step if that is bigger, plus overhead.
func PodGPUs(spec PodSpec) int {
	total := 0
	for _, name := range GPUResources {
		apps, sidecars, initPeak := 0, 0, 0
		for _, c := range spec.Containers {
			apps += c.Request(name)
		}
		for _, c := range spec.InitContainers {
			if c.RestartPolicy == "Always" {
				sidecars += c.Request(name)
				initPeak = max(initPeak, sidecars)
			} else {
				initPeak = max(initPeak, sidecars+c.Request(name))
			}
		}
		total += max(apps+sidecars, initPeak) + Count(spec.Overhead[name])
	}
	return total
}
