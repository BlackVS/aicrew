package server

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// routesFile is the route table as "METHOD path" lines. The documentation
// check (docs/examples_test.go) reads it, and this test pins it to the
// routes the service registers.
const routesFile = "routes.txt"

// TestRouteTableFile fails when routes.txt and the registered routes
// differ. AICREW_WRITE_ROUTES=1 rewrites the file from the routes.
func TestRouteTableFile(t *testing.T) {
	e := setupAPI(t)
	var lines []string
	for path, methods := range e.srv.routes {
		for m := range methods {
			lines = append(lines, m+" "+path)
		}
	}
	slices.SortFunc(lines, func(a, b string) int {
		_, pa, _ := strings.Cut(a, " ")
		_, pb, _ := strings.Cut(b, " ")
		if c := strings.Compare(pa, pb); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	want := strings.Join(lines, "\n") + "\n"
	if os.Getenv("AICREW_WRITE_ROUTES") == "1" {
		if err := os.WriteFile(routesFile, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(routesFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(got), "\r\n", "\n") != want {
		t.Fatalf("internal/server/%s is not the registered route table; rewrite it with\n"+
			"  AICREW_WRITE_ROUTES=1 go test -run '^TestRouteTableFile$' ./internal/server/\nwant:\n%s", routesFile, want)
	}
}
