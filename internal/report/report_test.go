package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vjsairam/inferaudit/internal/audit"
	"github.com/vjsairam/inferaudit/internal/collect"
)

func TestLabReport(t *testing.T) {
	snap, err := collect.FromDir("../../testdata/lab-2026-08-23")
	if err != nil {
		t.Fatal(err)
	}
	r := audit.Run(snap, audit.Options{Version: "test"})

	var page bytes.Buffer
	if err := HTML(&page, r); err != nil {
		t.Fatal(err)
	}
	html := page.String()
	for _, want := range []string{
		"The slowest pod took 7m 40s from creation to its latest Ready transition, 4m 38s of it pulling the image.",
		"Image pull took 4m 40s",
		`class="seg pull"`,
		`class="seg load"`,
		"Sample report.",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("report is missing %q", want)
		}
	}
	if strings.Contains(html, "ZgotmplZ") {
		t.Error("the template escaped a value it shouldn't have")
	}

	var out bytes.Buffer
	if err := JSON(&out, r); err != nil {
		t.Fatal(err)
	}
	var back audit.Report
	if err := json.Unmarshal(out.Bytes(), &back); err != nil || len(back.Findings) != len(r.Findings) {
		t.Errorf("JSON round trip: %v, %d findings", err, len(back.Findings))
	}
}

func TestEmptyReport(t *testing.T) {
	r := audit.Run(&collect.Snapshot{}, audit.Options{})
	var page bytes.Buffer
	if err := HTML(&page, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.String(), "No model-serving workloads found") {
		t.Error("empty report should say nothing was found")
	}
}
