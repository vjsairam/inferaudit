package kube

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSelectorMatches(t *testing.T) {
	labels := map[string]string{"app": "vllm", "tier": "serving"}
	tests := []struct {
		name string
		sel  *LabelSelector
		want bool
	}{
		{"nil selects nothing", nil, false},
		{"empty selects everything", &LabelSelector{}, true},
		{"match labels", &LabelSelector{MatchLabels: map[string]string{"app": "vllm"}}, true},
		{"wrong value", &LabelSelector{MatchLabels: map[string]string{"app": "tgi"}}, false},
		{"in", &LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: "tier", Operator: "In", Values: []string{"serving", "batch"}}}}, true},
		{"not in", &LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: "tier", Operator: "NotIn", Values: []string{"serving"}}}}, false},
		{"exists", &LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: "app", Operator: "Exists"}}}, true},
		{"does not exist", &LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: "app", Operator: "DoesNotExist"}}}, false},
		{"unknown operator", &LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: "app", Operator: "Gt"}}}, false},
	}
	for _, tt := range tests {
		if got := tt.sel.Matches(labels); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestProbeBudget(t *testing.T) {
	if got := (&Probe{}).Budget(); got != 30*time.Second {
		t.Errorf("defaults: got %s, want 30s", got)
	}
	if got := (&Probe{InitialDelaySeconds: 5, PeriodSeconds: 10, FailureThreshold: 90}).Budget(); got != 905*time.Second {
		t.Errorf("got %s, want 905s", got)
	}
}

func TestDecodeTolerance(t *testing.T) {
	var c Container
	if err := json.Unmarshal([]byte(`{"resources":{"limits":{"nvidia.com/gpu":1},"requests":{"nvidia.com/gpu":"2"}}}`), &c); err != nil {
		t.Fatal(err)
	}
	if got := c.GPUs(); got != 2 {
		t.Errorf("GPUs() = %d, want 2 (requests win over limits)", got)
	}

	spec := PodSpec{
		InitContainers: []Container{
			{Resources: Resources{Limits: map[string]Quantity{"nvidia.com/gpu": "2"}}},                            // one-off init step
			{RestartPolicy: "Always", Resources: Resources{Requests: map[string]Quantity{"nvidia.com/gpu": "1"}}}, // sidecar
		},
		Containers: []Container{{Resources: Resources{Requests: map[string]Quantity{"nvidia.com/gpu": "1"}}}},
	}
	if got := PodGPUs(spec); got != 2 {
		t.Errorf("PodGPUs = %d, want 2 (app plus sidecar, and the init step also peaks at 2)", got)
	}

	var e Event
	if err := json.Unmarshal([]byte(`{"firstTimestamp":null,"lastTimestamp":"","eventTime":"2026-08-23T14:09:50.761070Z"}`), &e); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 8, 23, 14, 9, 50, 761070000, time.UTC); !e.When().Equal(want) {
		t.Errorf("When() = %s, want %s", e.When(), want)
	}
}
