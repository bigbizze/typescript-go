package lintaiapi

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const strictConfig = `{
  "compilerOptions": {
    "target": "es2022",
    "module": "preserve",
    "moduleResolution": "bundler",
    "strict": true,
    "noEmit": true,
    "baseUrl": ".",
    "paths": { "@/*": ["src/*"] }
  },
  "include": ["src/**/*.ts"]
}
`

func buildTestSnapshot(t *testing.T, files map[string]string) *Snapshot {
	t.Helper()
	root := t.TempDir()
	if _, ok := files["tsconfig.json"]; !ok {
		files["tsconfig.json"] = strictConfig
	}
	paths := writeWorkspace(t, root, files)
	snapshot, err := BuildSnapshot(context.Background(), BuildSnapshotRequest{WorkspaceRoot: root, Files: paths})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func describeImport(edge ImportEdge) string {
	text := fmt.Sprintf("%s %s -> %s %s", edge.FromPath, edge.Specifier, edge.ToPath, edge.Form)
	if edge.Internal {
		text += " internal"
	}
	if edge.Unresolved {
		text += " unresolved"
	}
	if edge.Package != "" {
		text += " package=" + edge.Package
	}
	if edge.IsTypeOnly {
		text += " type"
	}
	return text
}

// Every import form makes an edge. An edge records its form, whether it
// resolves to a workspace source file, whether it resolves at all, and
// the package name for a bare specifier. Before this, only import
// declarations made edges, and an import that resolved into node_modules
// was marked internal.
func TestImportEdgesForEveryForm(t *testing.T) {
	snapshot := buildTestSnapshot(t, map[string]string{
		"node_modules/pkg/package.json": `{ "name": "pkg", "version": "1.0.0", "types": "index.d.ts" }`,
		"node_modules/pkg/index.d.ts":   "export declare function run(): void;\n",
		"node_modules/pkg/sub.d.ts":     "export declare const sub: number;\n",
		"src/types.d.ts":                "declare module \"virtual:env\" { export const mode: string; }\n",
		"src/b.ts":                      "export const b = 1;\nexport type B = number;\n",
		"src/a.ts": `import { b } from "./b";
import type { B } from "./b";
import { run } from "pkg";
import { sub } from "pkg/sub";
import { mode } from "virtual:env";
import { missing } from "./missing";
import { alias } from "@/typo";
export * from "./b";
export { b as renamed } from "./b";
export type { B as BType } from "./b";
import eq = require("./b");
const required = require("./b");
export async function lazy() { return import("./b"); }
export type Lazy = typeof import("./b");
export const used = [b, run, sub, mode, missing, alias, eq, required] as unknown as B;
`,
	})
	got := make([]string, 0, len(snapshot.ImportEdges))
	for _, edge := range snapshot.ImportEdges {
		got = append(got, describeImport(edge))
	}
	want := []string{
		"src/a.ts ./b -> src/b.ts static internal",
		"src/a.ts ./b -> src/b.ts static internal type",
		"src/a.ts ./b -> src/b.ts reexport internal",
		"src/a.ts ./b -> src/b.ts reexport internal",
		"src/a.ts ./b -> src/b.ts reexport internal type",
		"src/a.ts ./b -> src/b.ts import_equals internal",
		"src/a.ts ./b -> src/b.ts require internal",
		"src/a.ts ./b -> src/b.ts dynamic internal",
		"src/a.ts ./b -> src/b.ts import_type internal type",
		"src/a.ts ./missing -> ./missing static unresolved",
		"src/a.ts @/typo -> @/typo static unresolved",
		"src/a.ts pkg -> pkg static package=pkg",
		"src/a.ts pkg/sub -> pkg/sub static package=pkg",
		"src/a.ts virtual:env -> virtual:env static package=virtual:env",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("import edges differ:\ngot:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A method call links only to the declaration that the checker resolves.
// seen.add() on a Set and ids.text() on a parameter must not link to the
// local functions add and text. Module-scope calls and calls in anonymous
// default exports have a caller. Calls to declarations outside the
// workspace sources are external calls.
func TestCallEdgesFollowTheResolvedSignature(t *testing.T) {
	snapshot := buildTestSnapshot(t, map[string]string{
		"src/a.ts": `function add(value: number): number {
  return Date.now() + value;
}
const text = (value: number): string => String(value);
export function collect(values: number[]): Set<number> {
  const seen = new Set<number>();
  for (const value of values) seen.add(value);
  return seen;
}
export function show(ids: { text(id: string): string }, id: string): string {
  return ids.text(id);
}
export function direct(): string {
  return text(add(1));
}
export function parse(raw: string): unknown {
  return JSON.parse(raw);
}
class Box {
  open(): number { return add(2); }
  handler = () => this.open();
  get size(): number { return add(5); }
  [Symbol.iterator] = () => add(6);
}
export const box = new Box();
add(3);
export default () => add(4);
`,
	})
	calls := make([]string, 0)
	for _, edge := range snapshot.CallEdges {
		from := edge.FromName
		if edge.FromModule {
			from = "<module>"
		}
		calls = append(calls, fmt.Sprintf("%s -> %s", from, edge.ToName))
	}
	slices.Sort(calls)
	wantCalls := []string{
		"<module> -> add",
		"Box.[Symbol.iterator] -> add",
		"Box.handler -> Box.open",
		"Box.open -> add",
		"Box.size -> add",
		"default -> add",
		"direct -> add",
		"direct -> text",
	}
	if !slices.Equal(calls, wantCalls) {
		t.Fatalf("call edges differ:\ngot:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(wantCalls, "\n"))
	}

	external := make([]string, 0)
	for _, call := range snapshot.ExternalCalls {
		external = append(external, fmt.Sprintf("%s %s %s", call.FromSemanticKey, call.Module, call.Name))
	}
	slices.Sort(external)
	wantExternal := []string{
		"src/a.ts::arrow_function::text lib String",
		"src/a.ts::function::add lib DateConstructor.now",
		"src/a.ts::function::collect lib Set.add",
		"src/a.ts::function::parse lib JSON.parse",
	}
	if !slices.Equal(external, wantExternal) {
		t.Fatalf("external calls differ:\ngot:\n%s\nwant:\n%s", strings.Join(external, "\n"), strings.Join(wantExternal, "\n"))
	}
}

// Locations start at the first token. Before this, they started at the
// leading trivia, so import columns were one too small and a call after a
// comment started on an earlier line.
func TestLocationsStartAtTheFirstToken(t *testing.T) {
	snapshot := buildTestSnapshot(t, map[string]string{
		"src/b.ts": "export function b(): number { return 1; }\n",
		"src/a.ts": `import { b } from "./b";
export function a(): number {
  // comment

  return b();
}
`,
	})
	if len(snapshot.ImportEdges) != 1 || len(snapshot.CallEdges) != 1 {
		t.Fatalf("expected one import and one call, got %+v and %+v", snapshot.ImportEdges, snapshot.CallEdges)
	}
	importRange := snapshot.ImportEdges[0].Range
	if importRange.StartLine != 1 || importRange.StartColumn != 19 {
		t.Fatalf("import starts at %d:%d, want 1:19", importRange.StartLine, importRange.StartColumn)
	}
	callRange := snapshot.CallEdges[0].Range
	if callRange.StartLine != 5 || callRange.StartColumn != 10 {
		t.Fatalf("call starts at %d:%d, want 5:10", callRange.StartLine, callRange.StartColumn)
	}
}

func TestDirectivesAndParseErrors(t *testing.T) {
	snapshot := buildTestSnapshot(t, map[string]string{
		"src/client.ts": "'use client';\n\"use strict\";\nexport const x = 'not a directive';\n",
		"src/broken.ts": "export function f( {\n",
	})
	directives := make([]string, 0)
	for _, directive := range snapshot.Directives {
		directives = append(directives, fmt.Sprintf("%s %s %d:%d", directive.FilePath, directive.Text, directive.Range.StartLine, directive.Range.StartColumn))
	}
	slices.Sort(directives)
	want := []string{"src/client.ts use client 1:1", "src/client.ts use strict 2:1"}
	if !slices.Equal(directives, want) {
		t.Fatalf("directives differ: got %v, want %v", directives, want)
	}
	if len(snapshot.ParseErrors) != 1 || snapshot.ParseErrors[0].FilePath != "src/broken.ts" || snapshot.ParseErrors[0].Message == "" {
		t.Fatalf("expected one parse error in src/broken.ts, got %+v", snapshot.ParseErrors)
	}
}

// A global reference is a use of a name that resolves to the global binding.
// A local binding with the same name is not one.
func TestGlobalRefs(t *testing.T) {
	snapshot := buildTestSnapshot(t, map[string]string{
		"src/a.ts": `export function a() {
	const c = crypto;
	const w = globalThis.fetch;
	return [c, w, { Date }];
}
export function b(crypto: number) {
	const Date = 1;
	return crypto + Date;
}
`,
	})
	got := make([]string, 0)
	for _, ref := range snapshot.GlobalRefs {
		fn := ref.FunctionSemanticKey[strings.LastIndex(ref.FunctionSemanticKey, "::")+2:]
		got = append(got, fmt.Sprintf("%s %s:%d", ref.Name, fn, ref.Range.StartLine))
	}
	slices.Sort(got)
	want := []string{"Date a:4", "crypto a:2", "fetch a:3", "globalThis a:3"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// With verbatimModuleSyntax, import { type X } from "m" is emitted as
// import {} from "m", so the module loads at runtime. Without the option,
// tsc removes the import.
func TestRetainedTypeImports(t *testing.T) {
	for _, verbatim := range []bool{true, false} {
		config := strings.Replace(strictConfig, `"strict": true,`, fmt.Sprintf(`"strict": true, "verbatimModuleSyntax": %t,`, verbatim), 1)
		snapshot := buildTestSnapshot(t, map[string]string{
			"tsconfig.json": config,
			"src/m.ts":      "export type X = number;\nexport const y = 1;\n",
			"src/a.ts":      "import { type X } from \"./m\";\nimport type { X as Z } from \"./m\";\nimport { type X as W, y } from \"./m\";\nexport const v: X | Z | W = y;\n",
			"src/b.ts":      "export { type X } from \"./m\";\nexport type { X as Z } from \"./m\";\n",
		})
		retained := 0
		for _, edge := range snapshot.ImportEdges {
			if edge.Retained {
				retained++
				if edge.Range.StartLine != 1 {
					t.Fatalf("verbatim=%t: unexpected retained import %+v", verbatim, edge)
				}
			}
		}
		if want := map[bool]int{true: 2, false: 0}[verbatim]; retained != want {
			t.Fatalf("verbatim=%t: got %d retained imports, want %d", verbatim, retained, want)
		}
	}
}
