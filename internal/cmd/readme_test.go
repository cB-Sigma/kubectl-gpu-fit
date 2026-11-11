package cmd

import (
	"flag"
	"os"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the command output embedded in README.md")

// readmeBlock matches
//
//	<!-- BEGIN output: ARGS -->
//	```console
//	$ kubectl gpu-fit ARGS
//	...output...
//	```
//	<!-- END output -->
var readmeBlock = regexp.MustCompile("(?s)<!-- BEGIN output: (.*?) -->\n```console\n(.*?)```\n<!-- END output -->")

// TestReadmeOutput keeps the examples in README.md honest: every marked block
// must be exactly what the command prints. Run `make readme` to refresh.
func TestReadmeOutput(t *testing.T) {
	const path = "../../README.md"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	readme := string(data)
	blocks := readmeBlock.FindAllStringSubmatch(readme, -1)
	if len(blocks) == 0 {
		t.Fatal("no output blocks found in README.md")
	}
	updated := readme
	for _, b := range blocks {
		args := strings.Fields(b[1])
		res := run(t, nil, "", args...)
		if res.err != nil {
			t.Fatalf("kubectl gpu-fit %s: %v", b[1], res.err)
		}
		want := "$ kubectl gpu-fit " + b[1] + "\n" + res.stdout
		if b[2] == want {
			continue
		}
		if !*update {
			t.Errorf("README.md is out of date for `kubectl gpu-fit %s`; run `make readme`.\n--- README\n%s\n--- actual\n%s", b[1], b[2], want)
			continue
		}
		updated = strings.Replace(updated, b[0], "<!-- BEGIN output: "+b[1]+" -->\n```console\n"+want+"```\n<!-- END output -->", 1)
	}
	if *update && updated != readme {
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
