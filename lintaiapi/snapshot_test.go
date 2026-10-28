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

func TestBuildSnapshotExtractsExpandedMetadata(t *testing.T) {
	root := t.TempDir()
	files := writeWorkspace(t, root, map[string]string{
		"tsconfig.json": `{
  "compilerOptions": {
    "target": "es2022",
    "module": "nodenext",
    "moduleResolution": "nodenext",
    "noEmit": true
  },
  "include": ["src/**/*.ts"]
}
`,
		"src/data/db.ts": `export default async function db(query: string): Promise<number> {
	return query.length;
}

export type DbConfig = {
	dsn: string;
};

export function helper(config: DbConfig): number {
	return config.dsn.length;
}
`,
		"src/service/repository.ts": `import db, { helper as runHelper, type DbConfig } from "../data/db";
import * as dbModule from "../data/db";

type LocalConfig = DbConfig;

export class Repository {
	async save(config: DbConfig): Promise<number> {
		await db("select 1");
		return runHelper(config) + (await dbModule.default(config.dsn));
	}
}

export function mirror(config: LocalConfig): DbConfig {
	return config;
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

	var dbFunction *Function
	for i := range snapshot.Functions {
		if snapshot.Functions[i].Name == "db" {
			dbFunction = &snapshot.Functions[i]
			break
		}
	}
	if dbFunction == nil {
		t.Fatalf("expected db function in snapshot, got %+v", snapshot.Functions)
	}
	if !dbFunction.IsExported || !dbFunction.IsAsync {
		t.Fatalf("expected db function to be exported and async, got %+v", dbFunction)
	}
	if dbFunction.ParameterCount != 1 {
		t.Fatalf("expected db parameter count 1, got %+v", dbFunction)
	}
	if dbFunction.ReturnTypeText != "Promise<number>" {
		t.Fatalf("expected db return type Promise<number>, got %+v", dbFunction)
	}
	if len(dbFunction.ParameterTypeTexts) != 1 || dbFunction.ParameterTypeTexts[0] != "string" {
		t.Fatalf("unexpected db parameter types %+v", dbFunction.ParameterTypeTexts)
	}

	var mixedImport *ImportEdge
	var namespaceImport *ImportEdge
	for i := range snapshot.ImportEdges {
		edge := &snapshot.ImportEdges[i]
		if edge.FromPath != "src/service/repository.ts" {
			continue
		}
		if len(edge.ImportedSymbols) == 3 {
			mixedImport = edge
		}
		if len(edge.ImportedSymbols) == 1 && edge.ImportedSymbols[0].Kind == "namespace" {
			namespaceImport = edge
		}
	}
	if mixedImport == nil || namespaceImport == nil {
		t.Fatalf("expected mixed and namespace imports, got %+v", snapshot.ImportEdges)
	}
	if !mixedImport.HasDefaultImport || !mixedImport.HasNamedImports || mixedImport.HasNamespaceImport || mixedImport.IsTypeOnly {
		t.Fatalf("unexpected mixed import flags %+v", mixedImport)
	}
	if mixedImport.ImportedSymbols[0] != (ImportedSymbol{Name: "db", Kind: "default", IsTypeOnly: false}) {
		t.Fatalf("unexpected default import symbol %+v", mixedImport.ImportedSymbols[0])
	}
	if mixedImport.ImportedSymbols[1] != (ImportedSymbol{Name: "runHelper", Kind: "named", IsTypeOnly: false}) {
		t.Fatalf("unexpected named import symbol %+v", mixedImport.ImportedSymbols[1])
	}
	if mixedImport.ImportedSymbols[2] != (ImportedSymbol{Name: "DbConfig", Kind: "named", IsTypeOnly: true}) {
		t.Fatalf("unexpected type-only import symbol %+v", mixedImport.ImportedSymbols[2])
	}
	if !namespaceImport.HasNamespaceImport || namespaceImport.HasDefaultImport || namespaceImport.HasNamedImports || namespaceImport.IsTypeOnly {
		t.Fatalf("unexpected namespace import flags %+v", namespaceImport)
	}

	foundTarget := false
	for _, ref := range snapshot.TypeRefs {
		if ref.FilePath == "src/service/repository.ts" && ref.Name == "DbConfig" && ref.TargetPath == "src/data/db.ts" {
			foundTarget = true
			break
		}
	}
	if !foundTarget {
		t.Fatalf("expected DbConfig type ref target path, got %+v", snapshot.TypeRefs)
	}
}

func TestBuildSnapshotResolvesQualifiedTypeReferenceTargets(t *testing.T) {
	root := t.TempDir()
	files := writeWorkspace(t, root, map[string]string{
		"tsconfig.json": `{
  "compilerOptions": {
    "target": "es2022",
    "module": "nodenext",
    "moduleResolution": "nodenext",
    "noEmit": true
  },
  "include": ["src/**/*.ts"]
}
`,
		"src/models/types.ts": `export namespace Models {
	export type User = {
		id: string;
	};
}
`,
		"src/service/repository.ts": `import { Models } from "../models/types";

export function loadUser(user: Models.User): Models.User {
	return user;
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

	foundTarget := false
	for _, ref := range snapshot.TypeRefs {
		if ref.FilePath == "src/service/repository.ts" && ref.Name == "Models.User" && ref.TargetPath == "src/models/types.ts" {
			foundTarget = true
			break
		}
	}
	if !foundTarget {
		t.Fatalf("expected qualified type ref target path, got %+v", snapshot.TypeRefs)
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
