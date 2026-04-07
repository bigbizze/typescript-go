package lintaiapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/checker"
	"github.com/microsoft/typescript-go/internal/compiler"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/tsoptions"
	"github.com/microsoft/typescript-go/internal/vfs/osvfs"
)

type BuildSnapshotRequest struct {
	WorkspaceRoot string
	Files         []string
}

type SourceLocation struct {
	File        string
	StartLine   int
	StartColumn int
	EndLine     int
	EndColumn   int
}

type Module struct {
	EntityID    string
	SemanticKey string
	Path        string
	Range       SourceLocation
}

type Function struct {
	EntityID      string
	SemanticKey   string
	Name          string
	Kind          string
	FilePath      string
	ContainerName string
	ContainsAwait bool
	Range         SourceLocation
	BodyStart     int
	BodyEnd       int
}

type ImportEdge struct {
	EntityID    string
	SemanticKey string
	Specifier   string
	FromPath    string
	ToPath      string
	Range       SourceLocation
}

type CallEdge struct {
	EntityID        string
	SemanticKey     string
	FromSemanticKey string
	ToSemanticKey   string
	FromName        string
	ToName          string
	FromPath        string
	ToPath          string
	Range           SourceLocation
}

type TypeRef struct {
	EntityID    string
	SemanticKey string
	Name        string
	FilePath    string
	Range       SourceLocation
}

type Snapshot struct {
	Modules     []Module
	Functions   []Function
	ImportEdges []ImportEdge
	CallEdges   []CallEdge
	TypeRefs    []TypeRef
}

type configGroup struct {
	ConfigPath   string
	PrimaryFiles []string
}

type extractor struct {
	workspaceRoot  string
	program        *compiler.Program
	checker        *checker.Checker
	requestedFiles map[string]struct{}
	primaryFiles   map[string]struct{}
	filesByPath    map[string]*ast.SourceFile
	functionNodes  map[*ast.Node]*Function
	functionKeys   map[string]*Function
	functionSyms   map[*ast.Symbol]*Function
	functionCount  map[string]int
	importCount    map[string]int
	callCount      map[string]int
	typeRefCount   map[string]int
	functions      []Function
	imports        []ImportEdge
	calls          []CallEdge
	typeRefs       []TypeRef
}

type functionMeta struct {
	localName    string
	kind         string
	nameNode     *ast.Node
	namingSource *ast.Node
	lookupNodes  []*ast.Node
}

type functionState struct {
	record *Function
}

func BuildSnapshot(ctx context.Context, req BuildSnapshotRequest) (*Snapshot, error) {
	workspaceRoot, err := normalizeAbsPath(req.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	files, err := normalizeFiles(req.Files)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return &Snapshot{}, nil
	}

	groups, err := discoverConfigGroups(workspaceRoot, files)
	if err != nil {
		return nil, err
	}
	merged := &Snapshot{}
	missing := make(map[string]struct{})
	for _, group := range groups {
		partial, groupMissing, err := buildSnapshotForGroup(ctx, workspaceRoot, files, group)
		if err != nil {
			return nil, err
		}
		mergeSnapshot(merged, partial)
		for _, file := range groupMissing {
			missing[file] = struct{}{}
		}
	}
	if len(missing) > 0 {
		fallbackFiles := make([]string, 0, len(missing))
		for file := range missing {
			fallbackFiles = append(fallbackFiles, file)
		}
		slices.Sort(fallbackFiles)
		partial, _, err := buildSnapshotForGroup(ctx, workspaceRoot, files, configGroup{PrimaryFiles: fallbackFiles})
		if err != nil {
			return nil, err
		}
		mergeSnapshot(merged, partial)
	}
	sortSnapshot(merged)
	return merged, nil
}

func buildSnapshotForGroup(ctx context.Context, workspaceRoot string, requestedFiles []string, group configGroup) (*Snapshot, []string, error) {
	host := compiler.NewCompilerHost(
		workspaceRoot,
		bundled.WrapFS(osvfs.FS()),
		bundled.LibPath(),
		nil,
		nil,
	)
	config, err := loadConfig(host, workspaceRoot, group.PrimaryFiles, group.ConfigPath)
	if err != nil {
		return nil, nil, err
	}
	program := compiler.NewProgram(compiler.ProgramOptions{
		Config: config,
		Host:   host,
	})
	if diags := program.GetConfigFileParsingDiagnostics(); len(diags) > 0 {
		return nil, nil, fmt.Errorf("tsgo config diagnostics: %s", joinDiagnostics(diags))
	}
	program.BindSourceFiles()
	typeChecker, done := program.GetTypeChecker(ctx)
	defer done()

	ex := &extractor{
		workspaceRoot:  workspaceRoot,
		program:        program,
		checker:        typeChecker,
		requestedFiles: make(map[string]struct{}, len(requestedFiles)),
		primaryFiles:   make(map[string]struct{}, len(group.PrimaryFiles)),
		filesByPath:    make(map[string]*ast.SourceFile, len(requestedFiles)),
		functionNodes:  make(map[*ast.Node]*Function),
		functionKeys:   make(map[string]*Function),
		functionSyms:   make(map[*ast.Symbol]*Function),
		functionCount:  make(map[string]int),
		importCount:    make(map[string]int),
		callCount:      make(map[string]int),
		typeRefCount:   make(map[string]int),
	}
	for _, file := range requestedFiles {
		ex.requestedFiles[file] = struct{}{}
		if sourceFile := program.GetSourceFile(file); sourceFile != nil {
			ex.filesByPath[file] = sourceFile
		}
	}
	for _, file := range group.PrimaryFiles {
		ex.primaryFiles[file] = struct{}{}
	}

	modules := make([]Module, 0, len(group.PrimaryFiles))
	missingPrimary := make([]string, 0)
	for _, file := range group.PrimaryFiles {
		sourceFile := ex.filesByPath[file]
		if sourceFile == nil {
			missingPrimary = append(missingPrimary, file)
			continue
		}
		relative := ex.relativePath(file)
		modules = append(modules, Module{
			EntityID:    "module:" + relative,
			SemanticKey: fmt.Sprintf("%s::module", relative),
			Path:        relative,
			Range:       ex.moduleRange(sourceFile),
		})
	}

	for _, file := range requestedFiles {
		sourceFile := ex.filesByPath[file]
		if sourceFile == nil {
			continue
		}
		ex.walkFunctions(sourceFile.AsNode())
	}

	for _, file := range group.PrimaryFiles {
		sourceFile := ex.filesByPath[file]
		if sourceFile == nil {
			continue
		}
		ex.walkFacts(sourceFile.AsNode(), nil)
	}

	partial := &Snapshot{
		Modules:     modules,
		Functions:   ex.functions,
		ImportEdges: ex.imports,
		CallEdges:   ex.calls,
		TypeRefs:    ex.typeRefs,
	}
	sortSnapshot(partial)
	return partial, missingPrimary, nil
}

func loadConfig(host compiler.CompilerHost, workspaceRoot string, files []string, configPath string) (*tsoptions.ParsedCommandLine, error) {
	if configPath != "" {
		parsed, diags := tsoptions.GetParsedCommandLineOfConfigFile(configPath, &core.CompilerOptions{}, nil, host, nil)
		if len(diags) > 0 {
			return nil, fmt.Errorf("tsconfig diagnostics: %s", joinDiagnostics(diags))
		}
		return parsed, nil
	}

	syntheticPath := filepath.Join(workspaceRoot, "tsconfig.lintai.synthetic.json")
	relativeFiles := make([]any, 0, len(files))
	for _, file := range files {
		relative, err := filepath.Rel(workspaceRoot, filepath.FromSlash(file))
		if err != nil {
			return nil, err
		}
		relativeFiles = append(relativeFiles, filepath.ToSlash(relative))
	}
	jsonConfig := map[string]any{
		"files": relativeFiles,
		"compilerOptions": map[string]any{
			"allowJs":          true,
			"checkJs":          false,
			"noEmit":           true,
			"jsx":              "preserve",
			"target":           "es2022",
			"module":           "nodenext",
			"moduleResolution": "nodenext",
		},
	}
	parsed := tsoptions.ParseJsonConfigFileContent(
		jsonConfig,
		host,
		workspaceRoot,
		nil,
		filepath.ToSlash(syntheticPath),
		nil,
		nil,
		nil,
	)
	if diags := parsed.GetConfigFileParsingDiagnostics(); len(diags) > 0 {
		return nil, fmt.Errorf("synthetic tsconfig diagnostics: %s", joinDiagnostics(diags))
	}
	return parsed, nil
}

func discoverConfigGroups(workspaceRoot string, files []string) ([]configGroup, error) {
	grouped := make(map[string][]string)
	for _, file := range files {
		configPath, err := findNearestConfigPath(workspaceRoot, file)
		if err != nil {
			return nil, err
		}
		grouped[configPath] = append(grouped[configPath], file)
	}
	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	groups := make([]configGroup, 0, len(keys))
	for _, key := range keys {
		primaryFiles := grouped[key]
		slices.Sort(primaryFiles)
		groups = append(groups, configGroup{
			ConfigPath:   key,
			PrimaryFiles: primaryFiles,
		})
	}
	return groups, nil
}

func findNearestConfigPath(workspaceRoot string, file string) (string, error) {
	rootPath := filepath.Clean(filepath.FromSlash(workspaceRoot))
	current := filepath.Dir(filepath.FromSlash(file))
	for {
		candidate := filepath.Join(current, "tsconfig.json")
		if _, err := os.Stat(candidate); err == nil {
			return normalizeSlash(candidate), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if current == rootPath {
			return "", nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", nil
		}
		relative, err := filepath.Rel(rootPath, parent)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(relative, "..") {
			return "", nil
		}
		current = parent
	}
}

func mergeSnapshot(into *Snapshot, partial *Snapshot) {
	into.Modules = mergeModules(into.Modules, partial.Modules)
	into.Functions = mergeFunctions(into.Functions, partial.Functions)
	into.ImportEdges = mergeImportEdges(into.ImportEdges, partial.ImportEdges)
	into.CallEdges = mergeCallEdges(into.CallEdges, partial.CallEdges)
	into.TypeRefs = mergeTypeRefs(into.TypeRefs, partial.TypeRefs)
}

func sortSnapshot(snapshot *Snapshot) {
	slices.SortFunc(snapshot.Modules, func(a, b Module) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
	slices.SortFunc(snapshot.Functions, func(a, b Function) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
	slices.SortFunc(snapshot.ImportEdges, func(a, b ImportEdge) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
	slices.SortFunc(snapshot.CallEdges, func(a, b CallEdge) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
	slices.SortFunc(snapshot.TypeRefs, func(a, b TypeRef) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
}

func mergeModules(existing []Module, incoming []Module) []Module {
	seen := make(map[string]struct{}, len(existing))
	for _, item := range existing {
		seen[item.EntityID] = struct{}{}
	}
	for _, item := range incoming {
		if _, ok := seen[item.EntityID]; ok {
			continue
		}
		existing = append(existing, item)
		seen[item.EntityID] = struct{}{}
	}
	return existing
}

func mergeFunctions(existing []Function, incoming []Function) []Function {
	seen := make(map[string]struct{}, len(existing))
	for _, item := range existing {
		seen[item.EntityID] = struct{}{}
	}
	for _, item := range incoming {
		if _, ok := seen[item.EntityID]; ok {
			continue
		}
		existing = append(existing, item)
		seen[item.EntityID] = struct{}{}
	}
	return existing
}

func mergeImportEdges(existing []ImportEdge, incoming []ImportEdge) []ImportEdge {
	seen := make(map[string]struct{}, len(existing))
	for _, item := range existing {
		seen[item.EntityID] = struct{}{}
	}
	for _, item := range incoming {
		if _, ok := seen[item.EntityID]; ok {
			continue
		}
		existing = append(existing, item)
		seen[item.EntityID] = struct{}{}
	}
	return existing
}

func mergeCallEdges(existing []CallEdge, incoming []CallEdge) []CallEdge {
	seen := make(map[string]struct{}, len(existing))
	for _, item := range existing {
		seen[item.EntityID] = struct{}{}
	}
	for _, item := range incoming {
		if _, ok := seen[item.EntityID]; ok {
			continue
		}
		existing = append(existing, item)
		seen[item.EntityID] = struct{}{}
	}
	return existing
}

func mergeTypeRefs(existing []TypeRef, incoming []TypeRef) []TypeRef {
	seen := make(map[string]struct{}, len(existing))
	for _, item := range existing {
		seen[item.EntityID] = struct{}{}
	}
	for _, item := range incoming {
		if _, ok := seen[item.EntityID]; ok {
			continue
		}
		existing = append(existing, item)
		seen[item.EntityID] = struct{}{}
	}
	return existing
}

func (e *extractor) walkFunctions(node *ast.Node) {
	if node == nil {
		return
	}
	if ast.IsFunctionLike(node) {
		e.collectFunction(node)
	}
	node.ForEachChild(func(child *ast.Node) bool {
		e.walkFunctions(child)
		return false
	})
}

func (e *extractor) walkFacts(node *ast.Node, currentFunction *functionState) {
	if node == nil {
		return
	}
	switch {
	case ast.IsImportDeclaration(node):
		e.collectImport(node)
	case ast.IsTypeReferenceNode(node):
		e.collectTypeRef(node.AsTypeReferenceNode().TypeName, node.AsTypeReferenceNode().TypeName.AsNode())
	case ast.IsExpressionWithTypeArguments(node):
		expression := node.AsExpressionWithTypeArguments().Expression
		e.collectTypeRef(expression.AsNode(), expression.AsNode())
	case ast.IsCallExpression(node):
		if currentFunction != nil {
			e.collectCall(currentFunction.record, node)
		}
	}

	nextFunction := currentFunction
	if ast.IsFunctionLike(node) {
		nextFunction = nil
		if record := e.lookupFunction(node); record != nil {
			nextFunction = &functionState{record: record}
		}
	}

	node.ForEachChild(func(child *ast.Node) bool {
		e.walkFacts(child, nextFunction)
		return false
	})
}

func (e *extractor) collectFunction(node *ast.Node) *Function {
	if node.Body() == nil {
		return nil
	}
	meta := functionMetaForNode(node)
	if meta == nil || meta.localName == "" || meta.nameNode == nil {
		return nil
	}

	file := ast.GetSourceFileOfNode(node)
	if file == nil {
		return nil
	}
	fileName := normalizeSlash(file.FileName())
	if _, ok := e.requestedFiles[fileName]; !ok {
		return nil
	}

	containerName := containerNameForNode(node, meta.namingSource)
	displayName := meta.localName
	if containerName != "" {
		displayName = containerName + "." + meta.localName
	}

	nameRange := e.location(file, meta.nameNode.Pos(), meta.nameNode.End())
	baseKey := fmt.Sprintf("%s::%s::%s", nameRange.File, meta.kind, displayName)
	semanticKey := withOrdinal(baseKey, nextOrdinal(e.functionCount, baseKey))
	record := Function{
		EntityID:      "function:" + semanticKey,
		SemanticKey:   semanticKey,
		Name:          displayName,
		Kind:          meta.kind,
		FilePath:      nameRange.File,
		ContainerName: containerName,
		ContainsAwait: containsAwait(node),
		Range:         nameRange,
		BodyStart:     node.Body().Pos(),
		BodyEnd:       node.Body().End(),
	}
	e.functions = append(e.functions, record)
	stored := &e.functions[len(e.functions)-1]
	for _, lookupNode := range meta.lookupNodes {
		if lookupNode == nil {
			continue
		}
		e.functionNodes[lookupNode] = stored
		if key := nodeKey(lookupNode); key != "" {
			e.functionKeys[key] = stored
		}
		if symbol := lookupNode.Symbol(); symbol != nil {
			e.functionSyms[symbol] = stored
		}
	}
	if key := nodeKey(node); key != "" {
		e.functionKeys[key] = stored
	}
	if symbol := node.Symbol(); symbol != nil {
		e.functionSyms[symbol] = stored
	}
	return stored
}

func (e *extractor) collectImport(node *ast.Node) {
	file := ast.GetSourceFileOfNode(node)
	if file == nil {
		return
	}
	fileName := normalizeSlash(file.FileName())
	if _, ok := e.primaryFiles[fileName]; !ok {
		return
	}
	moduleSpecifier := node.ModuleSpecifier()
	if moduleSpecifier == nil || !ast.IsStringLiteralLike(moduleSpecifier) {
		return
	}
	specifier := moduleSpecifier.Text()
	fromPath := e.relativePath(fileName)
	toPath := specifier
	if resolved := e.program.GetResolvedModuleFromModuleSpecifier(file, moduleSpecifier); resolved != nil && resolved.IsResolved() {
		resolvedPath := normalizeSlash(resolved.ResolvedFileName)
		if isWithinRoot(e.workspaceRoot, resolvedPath) {
			toPath = e.relativePath(resolvedPath)
		}
	}
	location := e.location(file, moduleSpecifier.Pos(), moduleSpecifier.End())
	baseKey := fmt.Sprintf("%s::import::%s", fromPath, specifier)
	semanticKey := withOrdinal(baseKey, nextOrdinal(e.importCount, baseKey))
	e.imports = append(e.imports, ImportEdge{
		EntityID:    "import:" + semanticKey,
		SemanticKey: semanticKey,
		Specifier:   specifier,
		FromPath:    fromPath,
		ToPath:      toPath,
		Range:       location,
	})
}

func (e *extractor) collectCall(source *Function, node *ast.Node) {
	target := e.resolveCallTarget(node)
	if target == nil {
		return
	}
	file := ast.GetSourceFileOfNode(node)
	if file == nil {
		return
	}
	calleeNode := node.Expression()
	if ast.IsPropertyAccessExpression(calleeNode) {
		calleeNode = calleeNode.Name().AsNode()
	}
	location := e.location(file, calleeNode.Pos(), calleeNode.End())
	baseKey := fmt.Sprintf("%s::calls::%s", source.SemanticKey, target.SemanticKey)
	semanticKey := withOrdinal(baseKey, nextOrdinal(e.callCount, baseKey))
	e.calls = append(e.calls, CallEdge{
		EntityID:        "call:" + semanticKey,
		SemanticKey:     semanticKey,
		FromSemanticKey: source.SemanticKey,
		ToSemanticKey:   target.SemanticKey,
		FromName:        source.Name,
		ToName:          target.Name,
		FromPath:        source.FilePath,
		ToPath:          target.FilePath,
		Range:           location,
	})
}

func (e *extractor) resolveCallTarget(node *ast.Node) *Function {
	if signature := e.checker.GetResolvedSignature(node); signature != nil {
		if target := e.lookupFunction(signature.Declaration()); target != nil {
			return target
		}
	}
	expression := node.Expression()
	if expression == nil {
		return nil
	}
	if target := e.lookupFunction(e.checker.GetResolvedSymbol(expression)); target != nil {
		return target
	}
	if ast.IsPropertyAccessExpression(expression) {
		if target := e.lookupFunction(e.checker.GetResolvedSymbol(expression.Name().AsNode())); target != nil {
			return target
		}
	}
	if target := e.lookupFunction(e.checker.GetSymbolAtLocation(expression)); target != nil {
		return target
	}
	return nil
}

func (e *extractor) lookupFunction(value any) *Function {
	switch typed := value.(type) {
	case *ast.Node:
		if typed == nil {
			return nil
		}
		if record := e.functionNodes[typed]; record != nil {
			return record
		}
		if record := e.functionKeys[nodeKey(typed)]; record != nil {
			return record
		}
		if symbol := typed.Symbol(); symbol != nil {
			return e.functionSyms[symbol]
		}
	case *ast.Symbol:
		if typed == nil {
			return nil
		}
		for symbol := typed; symbol != nil; {
			if record := e.functionSyms[symbol]; record != nil {
				return record
			}
			if symbol.ValueDeclaration != nil {
				if record := e.functionNodes[symbol.ValueDeclaration]; record != nil {
					return record
				}
				if record := e.functionKeys[nodeKey(symbol.ValueDeclaration)]; record != nil {
					return record
				}
			}
			for _, decl := range symbol.Declarations {
				if record := e.functionNodes[decl]; record != nil {
					return record
				}
				if record := e.functionKeys[nodeKey(decl)]; record != nil {
					return record
				}
			}
			if symbol.Flags&ast.SymbolFlagsAlias == 0 {
				break
			}
			symbol = e.checker.GetImmediateAliasedSymbol(symbol)
		}
	}
	return nil
}

func (e *extractor) collectTypeRef(nameNode *ast.Node, rangeNode *ast.Node) {
	if nameNode == nil || rangeNode == nil {
		return
	}
	file := ast.GetSourceFileOfNode(nameNode)
	if file == nil {
		return
	}
	fileName := normalizeSlash(file.FileName())
	if _, ok := e.primaryFiles[fileName]; !ok {
		return
	}
	name := qualifiedName(nameNode)
	if name == "" {
		return
	}
	location := e.location(file, rangeNode.Pos(), rangeNode.End())
	baseKey := fmt.Sprintf("%s::type_ref::%s", location.File, name)
	semanticKey := withOrdinal(baseKey, nextOrdinal(e.typeRefCount, baseKey))
	e.typeRefs = append(e.typeRefs, TypeRef{
		EntityID:    "type_ref:" + semanticKey,
		SemanticKey: semanticKey,
		Name:        name,
		FilePath:    location.File,
		Range:       location,
	})
}

func (e *extractor) moduleRange(file *ast.SourceFile) SourceLocation {
	return e.location(file, 0, len(file.Text()))
}

func (e *extractor) location(file *ast.SourceFile, start int, end int) SourceLocation {
	relative := e.relativePath(file.FileName())
	if start < 0 || end < 0 {
		return SourceLocation{
			File:        relative,
			StartLine:   1,
			StartColumn: 1,
			EndLine:     1,
			EndColumn:   1,
		}
	}
	if end < start {
		end = start
	}
	lineStarts := file.ECMALineMap()
	startLine, startOffset := core.PositionToLineAndByteOffset(start, lineStarts)
	endLine, endOffset := core.PositionToLineAndByteOffset(end, lineStarts)
	return SourceLocation{
		File:        relative,
		StartLine:   startLine + 1,
		StartColumn: utf16Column(file.Text(), lineStarts, startLine, startOffset),
		EndLine:     endLine + 1,
		EndColumn:   utf16Column(file.Text(), lineStarts, endLine, endOffset),
	}
}

func (e *extractor) relativePath(fileName string) string {
	relative, err := filepath.Rel(filepath.FromSlash(e.workspaceRoot), filepath.FromSlash(fileName))
	if err != nil {
		return normalizeSlash(fileName)
	}
	return filepath.ToSlash(relative)
}

func containsAwait(node *ast.Node) bool {
	body := node.Body()
	if body == nil {
		return false
	}
	found := false
	var visit func(current *ast.Node, root bool)
	visit = func(current *ast.Node, root bool) {
		if current == nil || found {
			return
		}
		if !root && ast.IsFunctionLike(current) {
			return
		}
		if current.Kind == ast.KindAwaitExpression {
			found = true
			return
		}
		current.ForEachChild(func(child *ast.Node) bool {
			visit(child, false)
			return found
		})
	}
	visit(body, true)
	return found
}

func functionMetaForNode(node *ast.Node) *functionMeta {
	switch {
	case ast.IsFunctionDeclaration(node):
		name := node.Name()
		if name == nil {
			return nil
		}
		nameNode := name.AsNode()
		return &functionMeta{
			localName:    name.Text(),
			kind:         "function",
			nameNode:     nameNode,
			lookupNodes:  []*ast.Node{node, nameNode},
			namingSource: node,
		}
	case ast.IsConstructorDeclaration(node):
		return &functionMeta{
			localName:    "constructor",
			kind:         "constructor",
			nameNode:     node,
			lookupNodes:  []*ast.Node{node},
			namingSource: node,
		}
	case ast.IsMethodDeclaration(node):
		name := node.Name()
		if name == nil {
			return nil
		}
		nameNode := name.AsNode()
		return &functionMeta{
			localName:    ast.GetTextOfPropertyName(nameNode),
			kind:         "method",
			nameNode:     nameNode,
			lookupNodes:  []*ast.Node{node, nameNode},
			namingSource: node,
		}
	case ast.IsFunctionExpression(node):
		return expressionFunctionMeta(node, "function_expression")
	case ast.IsArrowFunction(node):
		return expressionFunctionMeta(node, "arrow_function")
	default:
		return nil
	}
}

func expressionFunctionMeta(node *ast.Node, kind string) *functionMeta {
	parent := node.Parent
	if parent == nil {
		return nil
	}
	if ast.IsVariableDeclaration(parent) && parent.Name() != nil {
		nameNode := parent.Name().AsNode()
		return &functionMeta{
			localName:    parent.Name().Text(),
			kind:         kind,
			nameNode:     nameNode,
			namingSource: parent,
			lookupNodes:  []*ast.Node{node, parent, nameNode},
		}
	}
	if ast.IsPropertyAssignment(parent) && parent.Name() != nil {
		nameNode := parent.Name().AsNode()
		return &functionMeta{
			localName:    ast.GetTextOfPropertyName(nameNode),
			kind:         kind,
			nameNode:     nameNode,
			namingSource: parent,
			lookupNodes:  []*ast.Node{node, parent, nameNode},
		}
	}
	name := node.Name()
	if name == nil {
		return nil
	}
	nameNode := name.AsNode()
	return &functionMeta{
		localName:    name.Text(),
		kind:         kind,
		nameNode:     nameNode,
		namingSource: node,
		lookupNodes:  []*ast.Node{node, nameNode},
	}
}

func containerNameForNode(node *ast.Node, namingSource *ast.Node) string {
	parts := make([]string, 0, 4)
	for child, parent := node, node.Parent; parent != nil; child, parent = parent, parent.Parent {
		if parent == namingSource {
			continue
		}
		switch {
		case ast.IsClassDeclaration(parent):
			if parent.Name() != nil {
				parts = append([]string{parent.Name().Text()}, parts...)
			}
		case ast.IsFunctionDeclaration(parent):
			if parent.Name() != nil {
				parts = append([]string{parent.Name().Text()}, parts...)
			}
		case ast.IsMethodDeclaration(parent):
			if parent.Name() != nil {
				parts = append([]string{ast.GetTextOfPropertyName(parent.Name().AsNode())}, parts...)
			}
		case ast.IsConstructorDeclaration(parent):
			parts = append([]string{"constructor"}, parts...)
		case ast.IsVariableDeclaration(parent):
			if parent.Name() != nil && parent.Initializer() == child && (ast.IsObjectLiteralExpression(child) || child.Kind == ast.KindClassExpression) {
				parts = append([]string{parent.Name().Text()}, parts...)
			}
		case ast.IsPropertyAssignment(parent):
			if parent.Name() != nil && parent.Initializer() == child && (ast.IsObjectLiteralExpression(child) || child.Kind == ast.KindClassExpression) {
				parts = append([]string{ast.GetTextOfPropertyName(parent.Name().AsNode())}, parts...)
			}
		}
	}
	return strings.Join(parts, ".")
}

func qualifiedName(node *ast.Node) string {
	switch {
	case node == nil:
		return ""
	case ast.IsIdentifier(node), ast.IsPrivateIdentifier(node):
		return node.Text()
	case ast.IsQualifiedName(node), ast.IsPropertyAccessExpression(node):
		return ast.EntityNameToString(node, nil)
	default:
		return ""
	}
}

func normalizeFiles(files []string) ([]string, error) {
	result := make([]string, 0, len(files))
	for _, file := range files {
		normalized, err := normalizeAbsPath(file)
		if err != nil {
			return nil, err
		}
		result = append(result, normalized)
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

func normalizeAbsPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return normalizeSlash(filepath.Clean(absolute)), nil
}

func normalizeSlash(path string) string {
	return filepath.ToSlash(path)
}

func isWithinRoot(root string, path string) bool {
	root = strings.TrimSuffix(root, "/")
	path = normalizeSlash(path)
	return path == root || strings.HasPrefix(path, root+"/")
}

func utf16Column(text string, lineStarts []core.TextPos, line int, byteOffset int) int {
	if line < 0 || line >= len(lineStarts) {
		return 1
	}
	start := int(lineStarts[line])
	end := start + byteOffset
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if end > len(text) {
		end = len(text)
	}
	return int(core.UTF16Len(text[start:end])) + 1
}

func joinDiagnostics(diags []*ast.Diagnostic) string {
	parts := make([]string, 0, len(diags))
	for _, diag := range diags {
		if diag == nil {
			continue
		}
		parts = append(parts, diag.String())
	}
	return strings.Join(parts, "; ")
}

func nodeKey(node *ast.Node) string {
	if node == nil {
		return ""
	}
	file := ast.GetSourceFileOfNode(node)
	if file == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d:%d", normalizeSlash(file.FileName()), node.Kind, node.Pos(), node.End())
}

func nextOrdinal(counts map[string]int, base string) int {
	counts[base]++
	return counts[base]
}

func withOrdinal(base string, ordinal int) string {
	if ordinal <= 1 {
		return base
	}
	return fmt.Sprintf("%s#%d", base, ordinal)
}
