// Command inferaudit reviews how AI models are served on a Kubernetes cluster and writes a
// report. It only reads, using `kubectl get`.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/vjsairam/inferaudit/internal/audit"
	"github.com/vjsairam/inferaudit/internal/collect"
	"github.com/vjsairam/inferaudit/internal/kube"
	"github.com/vjsairam/inferaudit/internal/report"
)

// version is set at release time with -ldflags "-X main.version=...". Builds from
// `go install module@version` fall back to the module version Go records in the binary.
var version = "dev"

const usage = `inferaudit reviews how AI models are served on a Kubernetes cluster.

It reads pods, workloads, Services, autoscalers, events and nodes with kubectl get,
then writes an HTML report and, if asked, JSON. Nothing in the cluster is changed.

Usage:
  inferaudit [flags]
  kubectl inferaudit [flags]     (installed as kubectl-inferaudit)

Flags:
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "inferaudit:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("inferaudit", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage)
		fs.PrintDefaults()
	}
	var (
		context   = fs.String("context", "", "kubeconfig context to use (default: the current one)")
		namespace = fs.String("n", "", "only look in this namespace (default: all namespaces)")
		from      = fs.String("from", "", "read a snapshot folder instead of the cluster")
		save      = fs.String("save", "", "also save what was read to this folder, for --from later")
		htmlOut   = fs.String("html", "inferaudit-report.html", "write the HTML report here (\"\" to skip)")
		jsonOut   = fs.String("json", "", "write the report as JSON here")
		selector  = fs.String("select", "", "also audit workloads whose pods carry these labels, e.g. app=llm,tier=serving")
		showVer   = fs.Bool("version", false, "print the version and exit")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (flags go before it, and --from takes a folder)", fs.Arg(0))
	}
	if *showVer {
		fmt.Println("inferaudit", buildVersion())
		return nil
	}
	if *htmlOut != "" && *jsonOut != "" && samePath(*htmlOut, *jsonOut) {
		return errors.New("--html and --json point at the same file")
	}
	if *from != "" && (*context != "" || *namespace != "" || *save != "") {
		return errors.New("--from reads a saved snapshot, so --context, -n and --save don't apply")
	}

	sel, err := parseSelector(*selector)
	if err != nil {
		return err
	}

	var snap *collect.Snapshot
	if *from != "" {
		snap, err = collect.FromDir(*from)
	} else {
		snap, err = collect.FromCluster(collect.Options{Context: *context, Namespace: *namespace, SaveDir: *save})
	}
	if err != nil {
		return err
	}

	r := audit.Run(snap, audit.Options{Version: buildVersion(), Selector: sel})

	if *htmlOut != "" {
		if err := writeFile(*htmlOut, func(f *os.File) error { return report.HTML(f, r) }); err != nil {
			return err
		}
	}
	if *jsonOut != "" {
		if err := writeFile(*jsonOut, func(f *os.File) error { return report.JSON(f, r) }); err != nil {
			return err
		}
	}

	fmt.Printf("%s, %s. %s worth checking, %s.\n",
		count(r.Totals.Workloads, "serving workload"), count(r.Totals.Pods, "pod"),
		count(r.Totals.Checks, "finding"), count(r.Totals.Notes, "note"))
	for _, g := range r.Meta.Gaps {
		fmt.Println("  gap:", g)
	}
	if *htmlOut != "" {
		fmt.Println("Report:", *htmlOut)
	}
	return nil
}

// parseSelector reads key=value pairs separated by commas.
func parseSelector(s string) (*kube.LabelSelector, error) {
	if s == "" {
		return nil, nil
	}
	sel := &kube.LabelSelector{MatchLabels: map[string]string{}}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--select wants key=value pairs, got %q", pair)
		}
		if _, dup := sel.MatchLabels[k]; dup {
			return nil, fmt.Errorf("--select names %q twice", k)
		}
		sel.MatchLabels[k] = v
	}
	return sel, nil
}

// writeFile writes to a temporary file and renames it into place, so a failed run leaves any
// earlier report intact. The file is readable only by the user, since reports name images,
// models and nodes.
func writeFile(path string, write func(*os.File) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".inferaudit-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := write(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

// samePath reports whether two output paths land on the same file. Each folder is resolved
// before any ".." is applied, as the OS does, so symlinked folders can't hide a clash.
func samePath(a, b string) bool {
	resolve := func(p string) string {
		dir, file := filepath.Split(p)
		if dir == "" {
			dir = "."
		}
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			dir = real
		}
		abs, err := filepath.Abs(filepath.Join(dir, file))
		if err != nil {
			return p
		}
		return abs
	}
	return resolve(a) == resolve(b)
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
