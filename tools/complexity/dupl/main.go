// dupl rolls a golangci-lint dupl JSON report up by feature, so the pipeline
// step the README describes actually exists in the tree.
//
//	golangci-lint run -c dupl-config.yaml --new=false --output.json.path=dupl.json ./...
//	dupl -in dupl.json -features features.json -root . -out dupl-by-feature.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"cxtool/internal/cx"
)

// golangciReport is the slice of golangci-lint's JSON output this tool reads.
type golangciReport struct {
	Issues []struct {
		Text string `json:"Text"`
		Pos  struct {
			Filename string `json:"Filename"`
			Line     int    `json:"Line"`
		} `json:"Pos"`
	} `json:"Issues"`
}

type pair struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Other     string `json:"other"`
	OtherLine int    `json:"other_line"`
	Span      int    `json:"span"`
}

type crossPair struct {
	A string `json:"a"`
	B string `json:"b"`
	N int    `json:"n"`
}

var (
	otherRe = regexp.MustCompile("`([^`:]+):(\\d+)-(\\d+)`")
	spanRe  = regexp.MustCompile(`^(\d+)-(\d+) lines`)
)

func main() {
	in := flag.String("in", "dupl.json", "golangci-lint JSON report (dupl linter)")
	featuresPath := flag.String("features", "features.json", "feature mapping")
	rootDir := flag.String("root", ".", "repository root, for normalizing the report's relative paths")
	out := flag.String("out", "dupl-by-feature.json", "output file")
	flag.Parse()

	cfg, err := cx.LoadConfig(*featuresPath)
	cx.Check(err)
	absRoot, err := filepath.Abs(*rootDir)
	cx.Check(err)

	b, err := os.ReadFile(*in)
	cx.Check(err)
	var rep golangciReport
	cx.Check(json.Unmarshal(b, &rep))

	// golangci emits paths relative to its own working directory (often a
	// chain of ../..); resolve against the repo root.
	norm := func(fn string) string {
		if !filepath.IsAbs(fn) {
			fn = filepath.Join(absRoot, fn)
		}
		if rel, err := filepath.Rel(absRoot, fn); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
		return filepath.ToSlash(fn)
	}

	prod := map[string]int{}
	lines := map[string]int{}
	test := map[string]int{}
	cross := map[[2]string]int{}
	var pairs []pair
	for _, it := range rep.Issues {
		fn := norm(it.Pos.Filename)
		feat := cfg.For(fn).Name
		span := 0
		if m := spanRe.FindStringSubmatch(it.Text); m != nil {
			a, _ := strconv.Atoi(m[1])
			b, _ := strconv.Atoi(m[2])
			span = b - a + 1
		}
		if strings.HasSuffix(fn, "_test.go") {
			test[feat]++
			continue
		}
		prod[feat]++
		lines[feat] += span
		if m := otherRe.FindStringSubmatch(it.Text); m != nil {
			other := norm(m[1])
			otherLine, _ := strconv.Atoi(m[2])
			pairs = append(pairs, pair{File: fn, Line: it.Pos.Line, Other: other, OtherLine: otherLine, Span: span})
			if of := cfg.For(other).Name; of != feat {
				k := [2]string{feat, of}
				if k[0] > k[1] {
					k[0], k[1] = k[1], k[0]
				}
				cross[k]++
			}
		}
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Span > pairs[j].Span })
	if len(pairs) > 40 {
		pairs = pairs[:40]
	}
	var crossOut []crossPair
	for k, n := range cross {
		crossOut = append(crossOut, crossPair{A: k[0], B: k[1], N: n})
	}
	sort.Slice(crossOut, func(i, j int) bool {
		if crossOut[i].N != crossOut[j].N {
			return crossOut[i].N > crossOut[j].N
		}
		return crossOut[i].A < crossOut[j].A
	})

	res := map[string]any{
		"total":            len(rep.Issues),
		"prod_by_feature":  prod,
		"lines_by_feature": lines,
		"test_by_feature":  test,
		"cross":            crossOut,
		"largest":          pairs,
	}
	j, err := json.MarshalIndent(res, "", " ")
	cx.Check(err)
	cx.Check(os.WriteFile(*out, j, 0o644))
	fmt.Fprintf(os.Stderr, "%d issues (%d prod, %d test) -> %s\n", len(rep.Issues), sum(prod), sum(test), *out)
}

func sum(m map[string]int) int {
	t := 0
	for _, v := range m {
		t += v
	}
	return t
}
