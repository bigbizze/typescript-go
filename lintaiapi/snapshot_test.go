package lintaiapi

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBuildSnapshotStableSemanticKeysAcrossTrivia(t *testing.T) {
	firstRoot := t.TempDir()
	firstFiles := writeWorkspace(t, firstRoot, map[string]string{
		"src/service/effect.ts": `export async function effect() {
	await Promise.resolve(1);
	return 1;
}
`,
		"src/pure/helper.ts": `import { effect } from "../service/effect";

type Value = Promise<number>;

export function helper(): Value {
	return effect();
}
`,
	})
	secondRoot := t.TempDir()
	secondFiles := writeWorkspace(t, secondRoot, map[string]string{
		"src/service/effect.ts": `// leading comment

export async function effect() {
	// interior comment
	await Promise.resolve(1);

	return 1;
}
`,
		"src/pure/helper.ts": `// import comment
import { effect } from "../service/effect";

// type comment
type Value = Promise<number>;

export function helper(): Value {
	// call comment
	return effect();
}
`,
	})

	firstSnapshot, err := BuildSnapshot(context.Background(), BuildSnapshotRequest{
		WorkspaceRoot: firstRoot,
		Files:         firstFiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondSnapshot, err := BuildSnapshot(context.Background(), BuildSnapshotRequest{
		WorkspaceRoot: secondRoot,
		Files:         secondFiles,
	})
	if err != nil {
		t.Fatal(err)
	}

	assertEqualKeys(t, moduleKeys(firstSnapshot), moduleKeys(secondSnapshot))
	assertEqualKeys(t, functionKeys(firstSnapshot), functionKeys(secondSnapshot))
	assertEqualKeys(t, importKeys(firstSnapshot), importKeys(secondSnapshot))
	assertEqualKeys(t, callKeys(firstSnapshot), callKeys(secondSnapshot))
	assertEqualKeys(t, typeRefKeys(firstSnapshot), typeRefKeys(secondSnapshot))
}

func TestBuildSnapshotUsesNearestTsConfigForAliasImports(t *testing.T) {
	root := t.TempDir()
	files := writeWorkspace(t, root, map[string]string{
		"packages/app/tsconfig.json": `{
  "compilerOptions": {
    "baseUrl": ".",
    "paths": {
      "@lib/*": ["src/lib/*"]
    },
    "target": "es2022",
    "module": "nodenext",
    "moduleResolution": "nodenext",
    "noEmit": true
  },
  "include": ["src/**/*.ts"]
}
`,
		"packages/app/src/lib/effect.ts": `export async function effect() {
	await Promise.resolve(1);
	return 1;
}
`,
		"packages/app/src/pure/helper.ts": `import { effect } from "@lib/effect";

export function helper() {
	return effect();
}
`,
	})

	snapshot, err := BuildSnapshot(context.Background(), BuildSnapshotRequest{
		WorkspaceRoot: root,
		Files:         files,
	})
	if err != nil {
		t.Fatal(err)
	}

	expectedImportTarget := "packages/app/src/lib/effect.ts"
	foundImport := false
	for _, edge := range snapshot.ImportEdges {
		if edge.FromPath == "packages/app/src/pure/helper.ts" && edge.ToPath == expectedImportTarget {
			foundImport = true
			break
		}
	}
	if !foundImport {
		t.Fatalf("expected alias import to resolve to %s, got %+v", expectedImportTarget, snapshot.ImportEdges)
	}

	foundCall := false
	for _, edge := range snapshot.CallEdges {
		if edge.FromName == "helper" && edge.ToName == "effect" {
			foundCall = true
			break
		}
	}
	if !foundCall {
		t.Fatalf("expected helper -> effect call edge, got %+v", snapshot.CallEdges)
	}
}

func writeWorkspace(t *testing.T, root string, files map[string]string) []string {
	t.Helper()
	paths := make([]string, 0, len(files))
	for relative, content := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if filepath.Ext(path) == ".ts" || filepath.Ext(path) == ".tsx" || filepath.Ext(path) == ".js" || filepath.Ext(path) == ".jsx" {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths
}

func assertEqualKeys(t *testing.T, left []string, right []string) {
	t.Helper()
	if !slices.Equal(left, right) {
		t.Fatalf("expected keys to match\nleft:  %v\nright: %v", left, right)
	}
}

func moduleKeys(snapshot *Snapshot) []string {
	keys := make([]string, 0, len(snapshot.Modules))
	for _, item := range snapshot.Modules {
		keys = append(keys, item.SemanticKey)
	}
	slices.Sort(keys)
	return keys
}

func functionKeys(snapshot *Snapshot) []string {
	keys := make([]string, 0, len(snapshot.Functions))
	for _, item := range snapshot.Functions {
		keys = append(keys, item.SemanticKey)
	}
	slices.Sort(keys)
	return keys
}

func importKeys(snapshot *Snapshot) []string {
	keys := make([]string, 0, len(snapshot.ImportEdges))
	for _, item := range snapshot.ImportEdges {
		keys = append(keys, item.SemanticKey)
	}
	slices.Sort(keys)
	return keys
}

func callKeys(snapshot *Snapshot) []string {
	keys := make([]string, 0, len(snapshot.CallEdges))
	for _, item := range snapshot.CallEdges {
		keys = append(keys, item.SemanticKey)
	}
	slices.Sort(keys)
	return keys
}

func typeRefKeys(snapshot *Snapshot) []string {
	keys := make([]string, 0, len(snapshot.TypeRefs))
	for _, item := range snapshot.TypeRefs {
		keys = append(keys, item.SemanticKey)
	}
	slices.Sort(keys)
	return keys
}
