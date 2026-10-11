// Package report writes an audit as a single self-contained HTML page or as JSON.
package report

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"time"

	"github.com/vjsairam/inferaudit/internal/audit"
)

//go:embed report.html.tmpl
var page string

var tmpl = template.Must(template.New("report").Funcs(template.FuncMap{
	"dur":   audit.Dur,
	"pct":   func(f float64) template.CSS { return template.CSS(fmt.Sprintf("%.2f%%", f)) },
	"clock": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
	"gpuShare": func(requested, allocatable int) float64 {
		if allocatable == 0 {
			return 0
		}
		return math.Min(100, 100*float64(requested)/float64(allocatable))
	},
}).Parse(page))

// JSON writes the report as indented JSON.
func JSON(w io.Writer, r *audit.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// HTML writes the report page.
func HTML(w io.Writer, r *audit.Report) error {
	return tmpl.Execute(w, build(r))
}

type view struct {
	*audit.Report
	Axis      []tick
	Workloads []workloadView
	Areas     []areaView
}

type tick struct {
	Pct   float64
	Label string
}

type workloadView struct {
	*audit.Workload
	Rows []row
}

type row struct {
	Pod, Node, Total, State string
	Restarts                int32
	Segments                []segment
	Timeline                *audit.Timeline
}

type segment struct {
	Class       string
	Left, Width float64
	Title       string
}

type areaView struct {
	Name     string
	Findings []audit.Finding
}

func build(r *audit.Report) view {
	v := view{Report: r}
	span := axisSpan(r)
	step := tickStep(span)
	for t := time.Duration(0); t <= span; t += step {
		v.Axis = append(v.Axis, tick{Pct: share(t, span), Label: tickLabel(t, step)})
	}
	for _, w := range r.Workloads {
		wv := workloadView{Workload: w}
		for _, t := range w.Timelines {
			wv.Rows = append(wv.Rows, makeRow(t, span))
		}
		v.Workloads = append(v.Workloads, wv)
	}
	for _, a := range audit.Areas {
		av := areaView{Name: a}
		for _, f := range r.Findings {
			if f.Area == a {
				av.Findings = append(av.Findings, f)
			}
		}
		if len(av.Findings) > 0 {
			v.Areas = append(v.Areas, av)
		}
	}
	return v
}

func makeRow(t *audit.Timeline, span time.Duration) row {
	r := row{Pod: t.Pod, Node: t.Node, State: t.State, Restarts: t.Restarts, Timeline: t}
	if d := t.Total(); d > 0 {
		r.Total = audit.Dur(d)
	}
	at := func(x time.Time) time.Duration { return x.Sub(t.Created) }
	add := func(class string, from, to time.Time, title string) {
		if from.IsZero() || to.IsZero() || !to.After(from) || from.Before(t.Created) {
			return
		}
		r.Segments = append(r.Segments, segment{
			Class: class, Left: share(at(from), span), Width: share(to.Sub(from), span), Title: title,
		})
	}
	if !t.PullStart.IsZero() {
		add("wait", t.Created, t.PullStart, "Before the image pull: "+audit.Dur(t.PullStart.Sub(t.Created)))
		title := "Image pull: " + audit.Dur(t.PullEnd.Sub(t.PullStart))
		if t.PullTook > 0 {
			title = "Image pull: the kubelet reported " + audit.Dur(t.PullTook)
		}
		add("pull", t.PullStart, t.PullEnd, title)
	}
	if t.Restarts == 0 {
		add("load", t.Started, t.Ready, "Container start to latest Ready: "+audit.Dur(t.StartToReady()))
	}
	return r
}

// axisSpan is the longest startup in the report, so every pod shares one time scale.
// After a restart only the pull is drawn, since start and Ready belong to a later start.
func axisSpan(r *audit.Report) time.Duration {
	var longest time.Duration
	for _, w := range r.Workloads {
		for _, t := range w.Timelines {
			ends := []time.Time{t.PullEnd}
			if t.Restarts == 0 {
				ends = append(ends, t.Started, t.Ready)
			}
			for _, end := range ends {
				if !end.IsZero() && !t.Created.IsZero() && end.After(t.Created) {
					longest = max(longest, end.Sub(t.Created))
				}
			}
		}
	}
	step := tickStep(longest)
	return time.Duration(math.Ceil(float64(longest)/float64(step))) * step
}

func tickStep(span time.Duration) time.Duration {
	switch {
	case span <= 2*time.Minute:
		return 15 * time.Second
	case span <= 10*time.Minute:
		return time.Minute
	case span <= 30*time.Minute:
		return 5 * time.Minute
	default: // about eight ticks, in whole minutes
		return max(10*time.Minute, time.Duration(math.Ceil(span.Minutes()/8))*time.Minute)
	}
}

func tickLabel(t, step time.Duration) string {
	switch {
	case t == 0:
		return "0"
	case step < time.Minute:
		return audit.Dur(t)
	case t >= time.Hour:
		return fmt.Sprintf("%dh %02dm", int(t.Hours()), int(t.Minutes())%60)
	default:
		return fmt.Sprintf("%dm", int(t.Minutes()))
	}
}

func share(d, span time.Duration) float64 {
	if span <= 0 {
		return 0
	}
	return math.Min(100, 100*float64(d)/float64(span))
}
