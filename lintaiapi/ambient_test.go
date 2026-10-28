package lintaiapi

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func testAmbientMatchers() []AmbientMatcher {
	zero := 0
	processModules := []AmbientModuleBinding{
		{Specifier: "node:process", Export: "default"},
		{Specifier: "node:process", Export: "env", Path: []string{"env"}},
		{Specifier: "process", Export: "default"},
		{Specifier: "process", Export: "env", Path: []string{"env"}},
	}
	return []AmbientMatcher{
		{Source: "clock", Capability: "now", Root: "Date", Path: []string{"now"}, Form: AmbientFormReference},
		{Source: "clock", Capability: "now", Root: "performance", Path: []string{"now"}, Form: AmbientFormReference, Modules: []AmbientModuleBinding{
			{Specifier: "node:perf_hooks", Export: "performance"},
			{Specifier: "perf_hooks", Export: "performance"},
		}},
		{Source: "clock", Capability: "construct_now", Root: "Date", Form: AmbientFormConstruct, ArgCount: &zero},
		{Source: "clock", Capability: "construct_now", Root: "Date", Form: AmbientFormCall},
		{Source: "env", Capability: "read", Root: "process", Path: []string{"env"}, Form: AmbientFormReference, KeyMember: true, Modules: processModules},
	}
}

func describeAmbient(use AmbientUse) string {
	key := "*"
	if use.HasKey {
		key = use.Key
	}
	if use.Source == "env" && use.HasKey == use.Dynamic || use.Source != "env" && (use.HasKey || use.Dynamic) {
		key = "invalid"
	}
	text := use.Source + "." + use.Capability + " " + key
	if use.Write {
		text += " write"
	}
	return text
}

func TestAmbientMatching(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   []string
	}{
		{"date-now", "export function a() { Date.now(); const f = Date.now; return f; }", []string{"clock.now *", "clock.now *"}},
		{"new-date", "export function a() { return new Date(); }", []string{"clock.construct_now *"}},
		{"new-date-arg", "export function a() { return new Date(0); }", nil},
		{"date-call", "export function a() { return Date(); }", []string{"clock.construct_now *"}},
		{"shadowed-date", "declare function makeClock(): { now(): number };\nexport function a() { const Date = makeClock(); return Date.now(); }", nil},
		{"performance-global", "export function a() { return performance.now(); }", []string{"clock.now *"}},
		{"performance-import", "import { performance } from \"node:perf_hooks\";\nexport function a() { return performance.now(); }", []string{"clock.now *"}},
		{"env-keys", "export function a() { return [process.env.A, process.env[\"B\"], process?.env?.C, globalThis.process.env.D]; }", []string{"env.read A", "env.read B", "env.read C", "env.read D"}},
		{"env-dynamic", "declare function f(value: unknown): void;\nexport function a(name: string) { process.env[name]; f(process.env); const copy = { ...process.env }; const e = process.env; return [copy, e]; }", []string{"env.read *", "env.read *", "env.read *", "env.read *"}},
		{"env-destructure", "export function a() { const { E, F: f, ...rest } = process.env; return [E, f, rest]; }", []string{"env.read *", "env.read E", "env.read F"}},
		{"env-in", "export function a() { return \"G\" in process.env; }", []string{"env.read G"}},
		{"env-write", "export function a() { process.env.H = \"x\"; delete process.env.I; }", []string{"env.read H write", "env.read I write"}},
		{"env-default-import", "import process from \"node:process\";\nexport function a() { return process.env.J; }", []string{"env.read J"}},
		{"env-named-import", "import { env } from \"node:process\";\nexport function a() { return env.K; }", []string{"env.read K"}},
		{"env-namespace-import", "import * as p from \"node:process\";\nexport function a() { return p.env.M; }", []string{"env.read M"}},
		{"env-parameter", "export function g(process: { env: Record<string, string> }) { return process.env.L; }", nil},
	}

	root := t.TempDir()
	files := map[string]string{}
	for _, tc := range cases {
		files["src/"+tc.name+".ts"] = tc.source
	}
	snapshot, err := BuildSnapshot(context.Background(), BuildSnapshotRequest{
		WorkspaceRoot:   root,
		Files:           writeWorkspace(t, root, files),
		AmbientMatchers: testAmbientMatchers(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, use := range snapshot.AmbientUses {
		got[use.FilePath] = append(got[use.FilePath], describeAmbient(use))
	}
	for _, tc := range cases {
		file := "src/" + tc.name + ".ts"
		actual := got[file]
		slices.Sort(actual)
		want := slices.Clone(tc.want)
		slices.Sort(want)
		if !slices.Equal(actual, want) {
			t.Errorf("%s: got %q, want %q", tc.name, actual, want)
		}
	}
}

func TestAmbientSitesAndCallsInsideCallbacksAttributeToEnclosingFunction(t *testing.T) {
	root := t.TempDir()
	files := writeWorkspace(t, root, map[string]string{
		"src/clock.ts": `export const startedAt = Date.now();

function stamp(value: number) {
	return value;
}

export function h(items: number[]) {
	return items.map(() => stamp(Date.now()));
}
`,
	})
	snapshot, err := BuildSnapshot(context.Background(), BuildSnapshotRequest{
		WorkspaceRoot:   root,
		Files:           files,
		AmbientMatchers: testAmbientMatchers(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.AmbientUses) != 2 {
		t.Fatalf("expected 2 ambient uses, got %+v", snapshot.AmbientUses)
	}
	var moduleScope, inFunction int
	for _, use := range snapshot.AmbientUses {
		switch {
		case use.FunctionSemanticKey == "":
			moduleScope++
			if use.Range.StartLine != 1 {
				t.Fatalf("expected module-scope use on line 1, got %+v", use)
			}
		case strings.HasSuffix(use.FunctionSemanticKey, "::function::h"):
			inFunction++
		default:
			t.Fatalf("unexpected function attribution %+v", use)
		}
	}
	if moduleScope != 1 || inFunction != 1 {
		t.Fatalf("expected one module-scope use and one use in h, got %+v", snapshot.AmbientUses)
	}
	foundCall := false
	for _, edge := range snapshot.CallEdges {
		if edge.FromName == "h" && edge.ToName == "stamp" {
			foundCall = true
		}
	}
	if !foundCall {
		t.Fatalf("expected call from h to stamp inside the callback, got %+v", snapshot.CallEdges)
	}
}

