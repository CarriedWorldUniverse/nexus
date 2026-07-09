// wormguard — self-contained CI fresh-dependency gate (Stux worm response).
//
// Fails if any THIRD-PARTY module in go.sum was published within the fortnight
// window and is not in an (optional) allowance ledger. GitHub-hosted runners
// can't reach the go-gate cluster service, so this enforces the same policy
// statically: the real CI worm vector is a PR pulling a fresh malicious dep,
// which runs its install hook in the runner with CI secrets. A fresh dep that
// hasn't been through allowance review fails here, before it is built/run.
//
// Own modules (GOPRIVATE globs) are skipped; pre-worm deps pass without a ledger
// entry; a module whose publish time can't be fetched fails closed. This is a
// vendored copy of `go-gate guard` (github.com/CarriedWorldUniverse/go-gate) so
// CI needs no cross-repo clone or credential; keep them in sync.
//
// Usage: wormguard -gosum go.sum [-allowlist allowance.txt] [-cutoff YYYY-MM-DD]
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

func main() {
	gosum := flag.String("gosum", "go.sum", "path to go.sum")
	allowPath := flag.String("allowlist", "", "optional allowance.txt; fresh deps must be listed to pass")
	cutoffStr := flag.String("cutoff", "", "fortnight cutoff YYYY-MM-DD (default 14d before now)")
	upstream := flag.String("upstream", "https://proxy.golang.org", "module proxy for publish times")
	privateCSV := flag.String("private", envOr("GOPRIVATE", "github.com/CarriedWorldUniverse/*"), "comma-separated GOPRIVATE-style globs to skip")
	conc := flag.Int("concurrency", 16, "parallel proxy lookups")
	flag.Parse()

	cutoff := time.Now().UTC().AddDate(0, 0, -14)
	if *cutoffStr != "" {
		t, err := time.Parse("2006-01-02", *cutoffStr)
		if err != nil {
			fatal("bad -cutoff: %v", err)
		}
		cutoff = t.UTC()
	}

	mods, err := modulesFromGoSum(*gosum)
	if err != nil {
		fatal("%v", err)
	}
	globs := splitNonEmpty(*privateCSV)
	allow := map[string]bool{}
	if *allowPath != "" {
		if allow, err = parseAllowance(*allowPath); err != nil {
			fatal("allowlist: %v", err)
		}
	}

	var third []moduleVersion
	skipped := 0
	for _, m := range mods {
		if matchesAnyGlob(m.module, globs) {
			skipped++
			continue
		}
		third = append(third, m)
	}

	results := checkFreshness(strings.TrimRight(*upstream, "/"), third, *conc)

	var fresh, unchecked []string
	for _, r := range results {
		mv := r.module + "@" + r.version
		switch {
		case r.err != "":
			unchecked = append(unchecked, mv+" ("+r.err+")")
		case r.published.Before(cutoff):
		case allow[mv]:
		default:
			fresh = append(fresh, fmt.Sprintf("%s (published %s)", mv, r.published.Format("2006-01-02")))
		}
	}
	sort.Strings(fresh)
	sort.Strings(unchecked)

	fmt.Printf("wormguard: %d third-party modules (%d private skipped), cutoff %s\n", len(third), skipped, cutoff.Format("2006-01-02"))
	for _, u := range unchecked {
		fmt.Println("  ? undateable (fail-closed):", u)
	}
	for _, f := range fresh {
		fmt.Println("  ! fresh + un-allowed:", f)
	}
	if len(fresh) == 0 && len(unchecked) == 0 {
		fmt.Println("OK: every dependency is pre-cutoff or an approved allowance entry.")
		return
	}
	fmt.Fprintln(os.Stderr, "\nFAIL: a fresh/undateable dependency must go through `go-gate vet` + operator approval into go-allowance before it can build.")
	os.Exit(1)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "wormguard: "+f+"\n", a...)
	os.Exit(2)
}

type moduleVersion struct{ module, version string }

func modulesFromGoSum(path string) ([]moduleVersion, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []moduleVersion
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) < 2 {
			continue
		}
		mod, ver := p[0], strings.TrimSuffix(p[1], "/go.mod")
		k := mod + "@" + ver
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, moduleVersion{mod, ver})
	}
	return out, sc.Err()
}

func parseAllowance(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		m[t] = true
	}
	return m, sc.Err()
}

func splitNonEmpty(csv string) []string {
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func matchesAnyGlob(module string, globs []string) bool {
	for _, g := range globs {
		g = strings.TrimSuffix(g, "/*")
		if module == g || strings.HasPrefix(module, g+"/") {
			return true
		}
	}
	return false
}

type freshResult struct {
	module, version string
	published       time.Time
	err             string
}

var client = &http.Client{Timeout: 60 * time.Second}

func checkFreshness(upstream string, mods []moduleVersion, conc int) []freshResult {
	res := make([]freshResult, len(mods))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, m := range mods {
		wg.Add(1)
		go func(i int, m moduleVersion) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := freshResult{module: m.module, version: m.version}
			if pub, err := publishTime(upstream, m.module, m.version); err != nil {
				r.err = err.Error()
			} else {
				r.published = pub
			}
			res[i] = r
		}(i, m)
	}
	wg.Wait()
	return res
}

func publishTime(upstream, module, version string) (time.Time, error) {
	resp, err := client.Get(upstream + "/" + encodePath(module) + "/@v/" + version + ".info")
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return time.Time{}, fmt.Errorf("proxy status %d", resp.StatusCode)
	}
	var info struct{ Time string }
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, info.Time)
	return t.UTC(), err
}

// encodePath applies the GOPROXY case encoding ("A" -> "!a").
func encodePath(module string) string {
	var b strings.Builder
	for i := 0; i < len(module); i++ {
		if c := module[i]; c >= 'A' && c <= 'Z' {
			b.WriteByte('!')
			b.WriteByte(c + ('a' - 'A'))
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}
