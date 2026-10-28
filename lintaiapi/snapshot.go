package lintaiapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/bundled"
	"github.com/microsoft/TypeScript/tsc/internal/checker"
	"github.com/microsoft/TypeScript/tsc/internal/compiler"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/module"
	"github.com/microsoft/TypeScript/tsc/internal/scanner"
	"github.com/microsoft/TypeScript/tsc/internal/tsoptions"
	"github.com/microsoft/TypeScript/tsc/internal/tspath"
	"github.com/microsoft/TypeScript/tsc/internal/vfs/osvfs"
)

const (
	ImportFormStatic       = "static"
	ImportFormReexport     = "reexport"
	ImportFormDynamic      = "dynamic"
	ImportFormRequire      = "require"
	ImportFormImportEquals = "import_equals"
	ImportFormImportType   = "import_type"
)

type BuildSnapshotRequest struct {
	WorkspaceRoot   string
	Files           []string
	AmbientMatchers []AmbientMatcher
	// TsconfigFiles limits the modules to the files that tsc compiles. A
	// file under a tsconfig.json is a module only when the program of that
	// tsconfig contains it, as a root file or as an imported file. When it
	// is false, every file in Files is a module, and a file that its
	// tsconfig does not contain is analyzed with a synthetic tsconfig.
	// Files with no tsconfig.json are always analyzed with the synthetic
	// tsconfig.
	TsconfigFiles bool
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
	EntityID           string
	SemanticKey        string
	Name               string
	Kind               string
	FilePath           string
	ContainerName      string
	ContainsAwait      bool
	IsExported         bool
	IsAsync            bool
	ParameterCount     int
	ReturnTypeText     string
	ParameterTypeTexts []string
	Range              SourceLocation
	BodyStart          int
	BodyEnd            int
}

type ImportedSymbol struct {
	Name       string
	Kind       string
	IsTypeOnly bool
}

type ImportEdge struct {
	EntityID           string
	SemanticKey        string
	Specifier          string
	FromPath           string
	ToPath             string
	Form               string
	Internal           bool
	Unresolved         bool
	Package            string
	ImportedSymbols    []ImportedSymbol
	HasDefaultImport   bool
	HasNamespaceImport bool
	HasNamedImports    bool
	IsTypeOnly         bool
	Range              SourceLocation
}

// CallEdge is a call from a collected function, or from module scope when
// FromModule is set, to a collected function. For a module-scope call,
// FromSemanticKey is the workspace-relative path of the module.
type CallEdge struct {
	EntityID        string
	SemanticKey     string
	FromSemanticKey string
	FromModule      bool
	ToSemanticKey   string
	FromName        string
	ToName          string
	FromPath        string
	ToPath          string
	Range           SourceLocation
}

// ExternalCall is a call whose resolved declaration is in a declaration file
// or outside the workspace: the default library, a package or a .d.ts file.
type ExternalCall struct {
	EntityID        string
	SemanticKey     string
	FromSemanticKey string
	FromModule      bool
	FromPath        string
	Module          string
	Name            string
	Range           SourceLocation
}

type ModuleDirective struct {
	EntityID    string
	SemanticKey string
	FilePath    string
	Text        string
	Range       SourceLocation
}

type ParseError struct {
	EntityID    string
	SemanticKey string
	FilePath    string
	Message     string
	Range       SourceLocation
}

type TypeRef struct {
	EntityID    string
	SemanticKey string
	Name        string
	FilePath    string
	TargetPath  string
	Range       SourceLocation
}

type PendingDtsCall struct {
	DtsPath         string
	TargetName      string
	FromSemanticKey string
	FromModule      bool
	FromName        string
	FromPath        string
	Range           SourceLocation
}

type Snapshot struct {
	Modules         []Module
	Functions       []Function
	ImportEdges     []ImportEdge
	CallEdges       []CallEdge
	ExternalCalls   []ExternalCall
	TypeRefs        []TypeRef
	AmbientUses     []AmbientUse
	Directives      []ModuleDirective
	ParseErrors     []ParseError
	PendingDtsCalls []PendingDtsCall
	// ConfigFiles are the absolute paths of the tsconfig files that the
	// snapshot read, including extended and referenced configs.
	ConfigFiles []string
}

type configGroup struct {
	ConfigPath   string
	Config       *tsoptions.ParsedCommandLine
	PrimaryFiles []string
}

type extractor struct {
	workspaceRoot   string
	program         *compiler.Program
	resolver        *module.DefaultResolver
	checker         *checker.Checker
	requestedFiles  map[string]struct{}
	primaryFiles    map[string]struct{}
	filesByPath     map[string]*ast.SourceFile
	functionNodes   map[*ast.Node]*Function
	functionKeys    map[string]*Function
	functionSyms    map[*ast.Symbol]*Function
	functionCount   map[string]int
	importCount     map[string]int
	callCount       map[string]int
	externalCount   map[string]int
	typeRefCount    map[string]int
	ambientCount    map[string]int
	functions       []Function
	imports         []ImportEdge
	calls           []CallEdge
	externalCalls   []ExternalCall
	typeRefs        []TypeRef
	ambientUses     []AmbientUse
	pendingDtsCalls []PendingDtsCall
	ambient         *ambientIndex
	ambientFile     *ambientFileState
	debugCalls      bool
}

type functionMeta struct {
	localName    string
	kind         string
	nameNode     *ast.Node
	namingSource *ast.Node
	lookupNodes  []*ast.Node
	exported     bool
}

type functionState struct {
	record *Function
}

// callSource is the caller of a call: a collected function, or module scope.
type callSource struct {
	key    string
	name   string
	path   string
	module bool
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

	host := compiler.NewCompilerHost(
		bundled.WrapFS(osvfs.FS()),
		bundled.LibPath(),
		nil,
		nil,
		nil,
	)
	configs := newConfigCache(host)
	groups, err := discoverConfigGroups(workspaceRoot, files, configs)
	if err != nil {
		return nil, err
	}
	merged := &Snapshot{}
	missing := make(map[string]struct{})
	for _, group := range groups {
		partial, groupMissing, err := buildSnapshotForGroup(ctx, host, workspaceRoot, files, group, req.AmbientMatchers)
		if err != nil {
			return nil, err
		}
		mergeSnapshot(merged, partial)
		if req.TsconfigFiles && group.Config != nil {
			continue
		}
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
		partial, _, err := buildSnapshotForGroup(ctx, host, workspaceRoot, files, configGroup{PrimaryFiles: fallbackFiles}, req.AmbientMatchers)
		if err != nil {
			return nil, err
		}
		mergeSnapshot(merged, partial)
	}
	resolveDtsCalls(merged)
	if req.TsconfigFiles {
		keepModuleFiles(merged)
	}
	merged.ConfigFiles = configs.files()
	sortSnapshot(merged)
	return merged, nil
}

// keepModuleFiles removes the functions in files that are not modules, and
// the call edges to them. A group can collect the functions of a file that
// it imports from another group, and that group can leave the file out.
// Import edges to such a file stay: the import is in the source.
func keepModuleFiles(snap *Snapshot) {
	modules := make(map[string]struct{}, len(snap.Modules))
	for _, item := range snap.Modules {
		modules[item.Path] = struct{}{}
	}
	isModule := func(path string) bool {
		_, ok := modules[path]
		return ok
	}
	snap.Functions = slices.DeleteFunc(snap.Functions, func(item Function) bool { return !isModule(item.FilePath) })
	snap.CallEdges = slices.DeleteFunc(snap.CallEdges, func(item CallEdge) bool { return !isModule(item.ToPath) })
}

// resolveDtsCalls resolves unresolved calls where the target declaration is
// in a .d.ts file but the source function exists in the snapshot under a
// different path (dist/ → src/ mapping). This happens in monorepos where
// packages consume each other's compiled output.
func resolveDtsCalls(snap *Snapshot) {
	// Build index of functions by (name, path)
	type fnKey struct{ name, path string }
	fnIndex := make(map[fnKey]int, len(snap.Functions))
	for i, fn := range snap.Functions {
		fnIndex[fnKey{fn.Name, fn.FilePath}] = i
	}

	// Process pending dts calls
	for _, pending := range snap.PendingDtsCalls {
		srcPath := mapDtsToSource(pending.DtsPath)
		if srcPath == "" {
			continue
		}
		idx, ok := fnIndex[fnKey{pending.TargetName, srcPath}]
		if !ok {
			continue
		}
		target := &snap.Functions[idx]
		baseKey := fmt.Sprintf("%s::calls::%s", pending.FromSemanticKey, target.SemanticKey)
		snap.CallEdges = append(snap.CallEdges, CallEdge{
			EntityID:        "call:" + baseKey,
			SemanticKey:     baseKey,
			FromSemanticKey: pending.FromSemanticKey,
			FromModule:      pending.FromModule,
			ToSemanticKey:   target.SemanticKey,
			FromName:        pending.FromName,
			ToName:          target.Name,
			FromPath:        pending.FromPath,
			ToPath:          target.FilePath,
			Range:           pending.Range,
		})
	}
	snap.PendingDtsCalls = nil
}

func buildSnapshotForGroup(ctx context.Context, host compiler.CompilerHost, workspaceRoot string, requestedFiles []string, group configGroup, ambientMatchers []AmbientMatcher) (*Snapshot, []string, error) {
	config := group.Config
	if config == nil {
		synthetic, err := syntheticConfig(host, workspaceRoot, group.PrimaryFiles)
		if err != nil {
			return nil, nil, err
		}
		config = synthetic
	}
	program := compiler.NewProgram(compiler.ProgramOptions{
		ProgramConfig: compiler.ProgramConfig{Config: config},
		ProgramHosts:  compiler.ProgramHosts{Host: host},
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
		externalCount:  make(map[string]int),
		typeRefCount:   make(map[string]int),
		ambientCount:   make(map[string]int),
		ambient:        newAmbientIndex(ambientMatchers),
		debugCalls:     os.Getenv("LINTAI_DEBUG_CALLS") == "1",
	}
	for _, file := range requestedFiles {
		ex.requestedFiles[file] = struct{}{}
		if sourceFile := program.GetSourceFile(tspath.RootedFilePathFromAbsolute(file)); sourceFile != nil {
			ex.filesByPath[file] = sourceFile
		}
	}
	for _, file := range group.PrimaryFiles {
		ex.primaryFiles[file] = struct{}{}
	}

	modules := make([]Module, 0, len(group.PrimaryFiles))
	directives := make([]ModuleDirective, 0)
	parseErrors := make([]ParseError, 0)
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
		directives = append(directives, ex.moduleDirectives(sourceFile, relative)...)
		if parseError, ok := ex.moduleParseError(sourceFile, relative); ok {
			parseErrors = append(parseErrors, parseError)
		}
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
		ex.beginAmbientFile(sourceFile)
		ex.walkFacts(sourceFile.AsNode(), nil)
	}
	ex.ambientFile = nil

	partial := &Snapshot{
		Modules:         modules,
		Functions:       ex.functions,
		ImportEdges:     ex.imports,
		CallEdges:       ex.calls,
		ExternalCalls:   ex.externalCalls,
		TypeRefs:        ex.typeRefs,
		AmbientUses:     ex.ambientUses,
		Directives:      directives,
		ParseErrors:     parseErrors,
		PendingDtsCalls: ex.pendingDtsCalls,
	}
	sortSnapshot(partial)
	return partial, missingPrimary, nil
}

// parsedConfig is a parsed tsconfig file, the set of its root files and the
// paths of its project references that exist.
type parsedConfig struct {
	command    *tsoptions.ParsedCommandLine
	rootFiles  map[string]struct{}
	references []string
}

type configCache struct {
	host    compiler.CompilerHost
	configs map[string]*parsedConfig
	errors  map[string]error
}

func newConfigCache(host compiler.CompilerHost) *configCache {
	return &configCache{host: host, configs: make(map[string]*parsedConfig), errors: make(map[string]error)}
}

func (c *configCache) load(configPath string) (*parsedConfig, error) {
	if parsed, ok := c.configs[configPath]; ok {
		return parsed, nil
	}
	if err, ok := c.errors[configPath]; ok {
		return nil, err
	}
	command, diags := tsoptions.GetParsedCommandLineOfConfigFile(tspath.RootedFilePathFromAbsolute(configPath), &core.CompilerOptions{}, nil, c.host.FS(), nil)
	if command != nil {
		diags = append(diags, command.GetConfigFileParsingDiagnostics()...)
	}
	if len(diags) > 0 {
		err := fmt.Errorf("tsconfig diagnostics: %s", joinDiagnostics(diags))
		c.errors[configPath] = err
		return nil, err
	}
	rootFiles := make(map[string]struct{}, len(command.FileNames()))
	for _, fileName := range command.FileNames() {
		rootFiles[normalizeSlash(fileName.AsString())] = struct{}{}
	}
	references := make([]string, 0, len(command.ProjectReferences()))
	for _, reference := range command.ResolvedProjectReferencePaths() {
		referencePath := normalizeSlash(reference.AsString())
		if c.host.FS().FileExists(reference) {
			references = append(references, referencePath)
		}
	}
	parsed := &parsedConfig{command: command, rootFiles: rootFiles, references: references}
	c.configs[configPath] = parsed
	return parsed, nil
}

// files returns the paths of every loaded config and of the configs that
// they extend or reference, sorted.
func (c *configCache) files() []string {
	result := make([]string, 0, len(c.configs))
	for configPath, parsed := range c.configs {
		result = append(result, configPath)
		for _, extended := range parsed.command.ExtendedSourceFiles() {
			result = append(result, normalizeSlash(extended.AsString()))
		}
		result = append(result, parsed.references...)
	}
	slices.Sort(result)
	return slices.Compact(result)
}

// owningConfig returns the config that lists file as a root file: the
// nearest config, or else the first config in its project references, in
// depth-first order. A solution-style tsconfig.json, with "files": [] and
// references, lists no files itself. When no config lists the file, it
// returns the nearest config. A reference to a config that does not exist
// is skipped.
func (c *configCache) owningConfig(nearestPath string, file string) (string, error) {
	visited := make(map[string]struct{})
	var search func(configPath string) (string, error)
	search = func(configPath string) (string, error) {
		if _, seen := visited[configPath]; seen {
			return "", nil
		}
		visited[configPath] = struct{}{}
		parsed, err := c.load(configPath)
		if err != nil {
			return "", err
		}
		if _, ok := parsed.rootFiles[file]; ok {
			return configPath, nil
		}
		for _, reference := range parsed.references {
			owner, err := search(reference)
			if err != nil || owner != "" {
				return owner, err
			}
		}
		return "", nil
	}
	owner, err := search(nearestPath)
	if err != nil || owner != "" {
		return owner, err
	}
	return nearestPath, nil
}

func syntheticConfig(host compiler.CompilerHost, workspaceRoot string, files []string) (*tsoptions.ParsedCommandLine, error) {
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
			"module":           "preserve",
			"moduleResolution": "bundler",
		},
	}
	parsed := tsoptions.ParseJsonConfigFileContent(
		jsonConfig,
		host.FS(),
		tspath.RootedDirectoryPathFromAbsolute(workspaceRoot),
		nil,
		tspath.RootedFilePathFromAbsolute(syntheticPath),
		nil,
		nil,
	)
	if diags := parsed.GetConfigFileParsingDiagnostics(); len(diags) > 0 {
		return nil, fmt.Errorf("synthetic tsconfig diagnostics: %s", joinDiagnostics(diags))
	}
	return parsed, nil
}

func discoverConfigGroups(workspaceRoot string, files []string, configs *configCache) ([]configGroup, error) {
	grouped := make(map[string][]string)
	for _, file := range files {
		configPath, err := findNearestConfigPath(workspaceRoot, file)
		if err != nil {
			return nil, err
		}
		if configPath != "" {
			configPath, err = configs.owningConfig(configPath, file)
			if err != nil {
				return nil, err
			}
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
		group := configGroup{ConfigPath: key, PrimaryFiles: primaryFiles}
		if key != "" {
			group.Config = configs.configs[key].command
		}
		groups = append(groups, group)
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
	into.Modules = mergeByEntityID(into.Modules, partial.Modules, func(item Module) string { return item.EntityID })
	into.Functions = mergeByEntityID(into.Functions, partial.Functions, func(item Function) string { return item.EntityID })
	into.ImportEdges = mergeByEntityID(into.ImportEdges, partial.ImportEdges, func(item ImportEdge) string { return item.EntityID })
	into.CallEdges = mergeByEntityID(into.CallEdges, partial.CallEdges, func(item CallEdge) string { return item.EntityID })
	into.ExternalCalls = mergeByEntityID(into.ExternalCalls, partial.ExternalCalls, func(item ExternalCall) string { return item.EntityID })
	into.TypeRefs = mergeByEntityID(into.TypeRefs, partial.TypeRefs, func(item TypeRef) string { return item.EntityID })
	into.AmbientUses = mergeByEntityID(into.AmbientUses, partial.AmbientUses, func(item AmbientUse) string { return item.EntityID })
	into.Directives = mergeByEntityID(into.Directives, partial.Directives, func(item ModuleDirective) string { return item.EntityID })
	into.ParseErrors = mergeByEntityID(into.ParseErrors, partial.ParseErrors, func(item ParseError) string { return item.EntityID })
	into.PendingDtsCalls = append(into.PendingDtsCalls, partial.PendingDtsCalls...)
}

func sortSnapshot(snapshot *Snapshot) {
	slices.SortFunc(snapshot.Modules, func(a, b Module) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
	slices.SortFunc(snapshot.Functions, func(a, b Function) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
	slices.SortFunc(snapshot.ImportEdges, func(a, b ImportEdge) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
	slices.SortFunc(snapshot.CallEdges, func(a, b CallEdge) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
	slices.SortFunc(snapshot.ExternalCalls, func(a, b ExternalCall) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
	slices.SortFunc(snapshot.TypeRefs, func(a, b TypeRef) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
	slices.SortFunc(snapshot.AmbientUses, func(a, b AmbientUse) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
	slices.SortFunc(snapshot.Directives, func(a, b ModuleDirective) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
	slices.SortFunc(snapshot.ParseErrors, func(a, b ParseError) int { return strings.Compare(a.SemanticKey, b.SemanticKey) })
}

func mergeByEntityID[T any](existing []T, incoming []T, entityID func(T) string) []T {
	seen := make(map[string]struct{}, len(existing))
	for _, item := range existing {
		seen[entityID(item)] = struct{}{}
	}
	for _, item := range incoming {
		id := entityID(item)
		if _, ok := seen[id]; ok {
			continue
		}
		existing = append(existing, item)
		seen[id] = struct{}{}
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
		e.collectImport(node, node.ModuleSpecifier(), ImportFormStatic, node.ImportClause() != nil && node.ImportClause().IsTypeOnly())
	case ast.IsExportDeclaration(node) && node.ModuleSpecifier() != nil:
		e.collectImport(node, node.ModuleSpecifier(), ImportFormReexport, node.IsTypeOnly())
	case ast.IsExternalModuleImportEqualsDeclaration(node):
		e.collectImport(node, node.AsImportEqualsDeclaration().ModuleReference.Expression(), ImportFormImportEquals, node.IsTypeOnly())
	case ast.IsLiteralImportTypeNode(node):
		e.collectImport(node, node.AsImportTypeNode().Argument.AsLiteralTypeNode().Literal, ImportFormImportType, true)
	case ast.IsImportCall(node):
		if arguments := node.Arguments(); len(arguments) > 0 {
			e.collectImport(node, arguments[0], ImportFormDynamic, false)
		}
	case ast.IsRequireCall(node, true):
		e.collectImport(node, node.Arguments()[0], ImportFormRequire, false)
	case ast.IsTypeReferenceNode(node):
		e.collectTypeRef(node.AsTypeReferenceNode().TypeName, node.AsTypeReferenceNode().TypeName.AsNode())
	case ast.IsExpressionWithTypeArguments(node):
		expression := node.AsExpressionWithTypeArguments().Expression
		e.collectTypeRef(expression.AsNode(), expression.AsNode())
	case ast.IsCallExpression(node):
		e.collectCall(e.callSourceFor(node, currentFunction), node)
	case ast.IsIdentifier(node):
		if e.ambientFile != nil {
			e.matchAmbient(node, currentFunction)
		}
	}

	nextFunction := currentFunction
	if ast.IsFunctionLike(node) {
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
	fileName := normalizeSlash(file.FileName().AsString())
	if _, ok := e.requestedFiles[fileName]; !ok {
		return nil
	}

	containerName := containerNameForNode(node, meta.namingSource)
	displayName := meta.localName
	if containerName != "" {
		displayName = containerName + "." + meta.localName
	}

	nameRange := e.nodeLocation(file, meta.nameNode)
	baseKey := fmt.Sprintf("%s::%s::%s", nameRange.File, meta.kind, displayName)
	semanticKey := withOrdinal(baseKey, nextOrdinal(e.functionCount, baseKey))
	record := Function{
		EntityID:           "function:" + semanticKey,
		SemanticKey:        semanticKey,
		Name:               displayName,
		Kind:               meta.kind,
		FilePath:           nameRange.File,
		ContainerName:      containerName,
		ContainsAwait:      containsAwait(node),
		IsExported:         meta.exported || e.isExportedFunction(node, meta),
		IsAsync:            ast.HasSyntacticModifier(node, ast.ModifierFlagsAsync),
		ParameterCount:     len(node.Parameters()),
		ReturnTypeText:     e.functionReturnTypeText(node),
		ParameterTypeTexts: e.functionParameterTypeTexts(node),
		Range:              nameRange,
		BodyStart:          node.Body().Pos(),
		BodyEnd:            node.Body().End(),
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

func (e *extractor) collectImport(node *ast.Node, moduleSpecifier *ast.Node, form string, typeOnly bool) {
	file := ast.GetSourceFileOfNode(node)
	if file == nil {
		return
	}
	fileName := normalizeSlash(file.FileName().AsString())
	if _, ok := e.primaryFiles[fileName]; !ok {
		return
	}
	if moduleSpecifier == nil || !ast.IsStringLiteralLike(moduleSpecifier) {
		return
	}
	specifier := moduleSpecifier.Text()
	fromPath := e.relativePath(fileName)
	toPath := specifier
	internal := false
	unresolved := false
	packageName := ""
	if resolved := e.resolveModuleSpecifier(file, moduleSpecifier); resolved.IsResolved() {
		resolvedPath := normalizeSlash(resolved.ResolvedFileName.AsString())
		if isWithinRoot(e.workspaceRoot, resolvedPath) && !strings.Contains(resolvedPath, "/node_modules/") {
			internal = true
			toPath = e.relativePath(resolvedPath)
			packageName = resolved.PackageId.Name
		} else {
			packageName = packageNameFromSpecifier(specifier)
		}
	} else if e.checker.ResolveExternalModuleName(moduleSpecifier, nil) != nil {
		packageName = packageNameFromSpecifier(specifier)
	} else {
		unresolved = true
	}
	location := e.nodeLocation(file, moduleSpecifier)
	baseKey := fmt.Sprintf("%s::import::%s", fromPath, specifier)
	semanticKey := withOrdinal(baseKey, nextOrdinal(e.importCount, baseKey))
	importedSymbols, hasDefaultImport, hasNamespaceImport, hasNamedImports := extractImportedSymbols(node, typeOnly)
	e.imports = append(e.imports, ImportEdge{
		EntityID:           "import:" + semanticKey,
		SemanticKey:        semanticKey,
		Specifier:          specifier,
		FromPath:           fromPath,
		ToPath:             toPath,
		Form:               form,
		Internal:           internal,
		Unresolved:         unresolved,
		Package:            packageName,
		ImportedSymbols:    importedSymbols,
		HasDefaultImport:   hasDefaultImport,
		HasNamespaceImport: hasNamespaceImport,
		HasNamedImports:    hasNamedImports,
		IsTypeOnly:         typeOnly,
		Range:              location,
	})
}

// resolveModuleSpecifier returns the program's resolution of a specifier. The
// program does not resolve some specifiers, for example require() calls in
// TypeScript files, so it falls back to the program's resolver.
func (e *extractor) resolveModuleSpecifier(file *ast.SourceFile, moduleSpecifier *ast.Node) *module.ResolvedModule {
	if resolved := e.program.GetResolvedModuleFromModuleSpecifier(file, moduleSpecifier); resolved.IsResolved() {
		return resolved
	}
	mode := e.program.GetModeForUsageLocation(file, moduleSpecifier)
	if e.resolver == nil {
		e.resolver = e.program.NewModuleResolver()
	}
	resolved, _, _ := e.resolver.ResolveModuleName(moduleSpecifier.Text(), file.FileName(), mode, nil)
	return resolved
}

// packageNameFromSpecifier returns the package part of a bare specifier:
// "next" for "next/server" and "@scope/pkg" for "@scope/pkg/sub".
func packageNameFromSpecifier(specifier string) string {
	if specifier == "" || tspath.IsExternalModuleNameRelative(specifier) {
		return ""
	}
	parts := strings.Split(specifier, "/")
	if strings.HasPrefix(specifier, "@") && len(parts) > 1 {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

func (e *extractor) callSourceFor(node *ast.Node, currentFunction *functionState) callSource {
	if currentFunction != nil {
		record := currentFunction.record
		return callSource{key: record.SemanticKey, name: record.Name, path: record.FilePath}
	}
	relative := e.relativePath(normalizeSlash(ast.GetSourceFileOfNode(node).FileName().AsString()))
	return callSource{key: relative, name: relative, path: relative, module: true}
}

func (e *extractor) collectCall(source callSource, node *ast.Node) {
	file := ast.GetSourceFileOfNode(node)
	if file == nil {
		return
	}
	if _, ok := e.primaryFiles[normalizeSlash(file.FileName().AsString())]; !ok {
		return
	}
	target, declaration := e.resolveCallTarget(node)
	location := e.nodeLocation(file, calleeNameNode(node))
	if target == nil {
		if declaration == nil || e.recordPendingDtsCall(source, declaration, location) {
			return
		}
		e.recordExternalCall(source, node, declaration, location)
		return
	}
	baseKey := fmt.Sprintf("%s::calls::%s", source.key, target.SemanticKey)
	semanticKey := withOrdinal(baseKey, nextOrdinal(e.callCount, baseKey))
	e.calls = append(e.calls, CallEdge{
		EntityID:        "call:" + semanticKey,
		SemanticKey:     semanticKey,
		FromSemanticKey: source.key,
		FromModule:      source.module,
		ToSemanticKey:   target.SemanticKey,
		FromName:        source.name,
		ToName:          target.Name,
		FromPath:        source.path,
		ToPath:          target.FilePath,
		Range:           location,
	})
}

func calleeNameNode(node *ast.Node) *ast.Node {
	callee := ast.SkipParentheses(node.Expression())
	if ast.IsPropertyAccessExpression(callee) {
		return callee.Name().AsNode()
	}
	return callee
}

// resolveCallTarget returns the collected function that a call resolves to.
// When it finds none, it returns the declaration of the checker's resolved
// signature, if there is one. A call links to a function only through the
// checker's signature or through the callee's own symbol, never through a
// name lookup, so seen.add() on a Set does not link to a local add.
func (e *extractor) resolveCallTarget(node *ast.Node) (*Function, *ast.Node) {
	var declaration *ast.Node
	if signature := e.checker.GetResolvedSignature(node); signature != nil {
		declaration = signature.Declaration()
		if target := e.lookupFunction(declaration); target != nil {
			return target, nil
		}
		if declaration != nil {
			if target := e.lookupFunction(declaration.Symbol()); target != nil {
				return target, nil
			}
		}
	}
	expression := ast.SkipParentheses(node.Expression())
	if expression != nil && (ast.IsIdentifier(expression) || ast.IsPropertyAccessExpression(expression)) {
		if target := e.lookupFunction(e.checker.GetSymbolAtLocation(expression)); target != nil {
			return target, nil
		}
	}
	return nil, declaration
}

// recordPendingDtsCall records a call to a declaration in a workspace dist/
// .d.ts file. resolveDtsCalls later links it to the matching source function.
func (e *extractor) recordPendingDtsCall(source callSource, decl *ast.Node, location SourceLocation) bool {
	sf := ast.GetSourceFileOfNode(decl)
	if sf == nil {
		return false
	}
	fileName := normalizeSlash(sf.FileName().AsString())
	if !strings.HasSuffix(fileName, ".d.ts") || !isWithinRoot(e.workspaceRoot, fileName) || strings.Contains(fileName, "/node_modules/") {
		return false
	}
	name := ""
	if decl.Symbol() != nil {
		name = decl.Symbol().Name()
	}
	if name == "" {
		return false
	}
	relPath := e.relativePath(fileName)
	if !strings.Contains(relPath, "/dist/") {
		return false
	}
	e.pendingDtsCalls = append(e.pendingDtsCalls, PendingDtsCall{
		DtsPath:         relPath,
		TargetName:      name,
		FromSemanticKey: source.key,
		FromModule:      source.module,
		FromName:        source.name,
		FromPath:        source.path,
		Range:           location,
	})
	return true
}

// recordExternalCall records a call whose declaration is in the default
// library, in a package, or in a declaration file. Calls to declarations in
// workspace source files, such as interface members, are not external.
func (e *extractor) recordExternalCall(source callSource, call *ast.Node, decl *ast.Node, location SourceLocation) {
	moduleName := e.externalModuleName(decl)
	name := qualifiedSymbolName(decl.Symbol())
	if name == "" {
		callee := e.checker.GetSymbolAtLocation(ast.SkipParentheses(call.Expression()))
		if callee != nil && callee.Flags()&ast.SymbolFlagsAlias != 0 {
			callee = e.checker.SkipAlias(callee)
		}
		name = qualifiedSymbolName(callee)
	}
	if moduleName == "" || name == "" {
		return
	}
	baseKey := fmt.Sprintf("%s::external::%s::%s", source.key, moduleName, name)
	semanticKey := withOrdinal(baseKey, nextOrdinal(e.externalCount, baseKey))
	e.externalCalls = append(e.externalCalls, ExternalCall{
		EntityID:        "external_call:" + semanticKey,
		SemanticKey:     semanticKey,
		FromSemanticKey: source.key,
		FromModule:      source.module,
		FromPath:        source.path,
		Module:          moduleName,
		Name:            name,
		Range:           location,
	})
}

// externalModuleName names the module that declares an external
// declaration: the name of an enclosing declare module "x" block, "lib" for
// the default library, the package name for node_modules, or the
// workspace-relative path of a declaration file. It returns "" for
// declarations in workspace source files.
func (e *extractor) externalModuleName(decl *ast.Node) string {
	for parent := decl.Parent; parent != nil; parent = parent.Parent {
		if ast.IsModuleDeclaration(parent) && ast.IsStringLiteral(parent.Name()) {
			return parent.Name().Text()
		}
	}
	file := ast.GetSourceFileOfNode(decl)
	if file == nil {
		return ""
	}
	if e.program.IsSourceFileDefaultLibrary(file.PathKey()) {
		return "lib"
	}
	fileName := normalizeSlash(file.FileName().AsString())
	if index := strings.LastIndex(fileName, "/node_modules/"); index >= 0 {
		packageName := packageNameFromSpecifier(fileName[index+len("/node_modules/"):])
		if typed, ok := strings.CutPrefix(packageName, "@types/"); ok {
			if scope, name, scoped := strings.Cut(typed, "__"); scoped {
				return "@" + scope + "/" + name
			}
			return typed
		}
		return packageName
	}
	if !file.IsDeclarationFile && isWithinRoot(e.workspaceRoot, fileName) {
		return ""
	}
	return e.relativePath(fileName)
}

// qualifiedSymbolName returns the name of a symbol with the names of its
// containing classes, interfaces and namespaces, for example "JSON.parse"
// or "Set.add". It returns "" for a symbol with an internal name, such as a
// call signature.
func qualifiedSymbolName(symbol *ast.Symbol) string {
	if symbol == nil || isInternalSymbolName(symbol.Name()) {
		return ""
	}
	parts := []string{symbol.Name()}
	for parent := symbol.Parent(); parent != nil; parent = parent.Parent() {
		name := parent.Name()
		if name == "" || strings.HasPrefix(name, "\"") || isInternalSymbolName(name) {
			break
		}
		parts = append([]string{name}, parts...)
	}
	return strings.Join(parts, ".")
}

func isInternalSymbolName(name string) bool {
	return strings.HasPrefix(name, ast.InternalSymbolNamePrefix)
}

// moduleDirectives returns the directive prologue of a file, for example
// "use client".
func (e *extractor) moduleDirectives(file *ast.SourceFile, relative string) []ModuleDirective {
	result := make([]ModuleDirective, 0)
	counts := make(map[string]int)
	for _, statement := range file.Statements.Nodes {
		if !ast.IsPrologueDirective(statement) {
			break
		}
		expression := statement.Expression()
		text := expression.Text()
		baseKey := fmt.Sprintf("%s::directive::%s", relative, text)
		semanticKey := withOrdinal(baseKey, nextOrdinal(counts, baseKey))
		result = append(result, ModuleDirective{
			EntityID:    "directive:" + semanticKey,
			SemanticKey: semanticKey,
			FilePath:    relative,
			Text:        text,
			Range:       e.nodeLocation(file, expression),
		})
	}
	return result
}

// moduleParseError returns the first syntax error of a file.
func (e *extractor) moduleParseError(file *ast.SourceFile, relative string) (ParseError, bool) {
	diagnostics := file.Diagnostics()
	if len(diagnostics) == 0 {
		return ParseError{}, false
	}
	first := diagnostics[0]
	semanticKey := relative + "::parse_error"
	return ParseError{
		EntityID:    "parse_error:" + semanticKey,
		SemanticKey: semanticKey,
		FilePath:    relative,
		Message:     first.String(),
		Range:       e.location(file, first.Pos(), first.End()),
	}, true
}

func mapDtsToSource(dtsPath string) string {
	// packages/foo/dist/bar.d.ts → packages/foo/src/bar.ts
	if !strings.Contains(dtsPath, "/dist/") {
		return ""
	}
	srcPath := strings.Replace(dtsPath, "/dist/", "/src/", 1)
	srcPath = strings.TrimSuffix(srcPath, ".d.ts") + ".ts"
	return srcPath
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
		// Try the symbol as-is and then its fully-resolved alias.
		candidates := []*ast.Symbol{typed}
		if typed.Flags()&ast.SymbolFlagsAlias != 0 {
			if resolved := e.checker.SkipAlias(typed); resolved != nil && resolved != typed {
				candidates = append(candidates, resolved)
			}
		}
		for _, symbol := range candidates {
			if record := e.functionSyms[symbol]; record != nil {
				return record
			}
			if symbol.ValueDeclaration() != nil {
				if record := e.functionNodes[symbol.ValueDeclaration()]; record != nil {
					return record
				}
				if record := e.functionKeys[nodeKey(symbol.ValueDeclaration())]; record != nil {
					return record
				}
			}
			for _, decl := range symbol.Declarations() {
				if record := e.functionNodes[decl]; record != nil {
					return record
				}
				if record := e.functionKeys[nodeKey(decl)]; record != nil {
					return record
				}
			}
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
	fileName := normalizeSlash(file.FileName().AsString())
	if _, ok := e.primaryFiles[fileName]; !ok {
		return
	}
	name := qualifiedName(nameNode)
	if name == "" {
		return
	}
	location := e.nodeLocation(file, rangeNode)
	baseKey := fmt.Sprintf("%s::type_ref::%s", location.File, name)
	semanticKey := withOrdinal(baseKey, nextOrdinal(e.typeRefCount, baseKey))
	e.typeRefs = append(e.typeRefs, TypeRef{
		EntityID:    "type_ref:" + semanticKey,
		SemanticKey: semanticKey,
		Name:        name,
		FilePath:    location.File,
		TargetPath:  e.resolveTypeTargetPath(nameNode),
		Range:       location,
	})
}

func (e *extractor) isExportedFunction(node *ast.Node, meta *functionMeta) bool {
	if ast.GetCombinedModifierFlags(meta.namingSource)&ast.ModifierFlagsExport != 0 {
		return true
	}
	for _, lookupNode := range meta.lookupNodes {
		if lookupNode == nil {
			continue
		}
		if symbol := lookupNode.Symbol(); e.isExportedSymbol(symbol) {
			return true
		}
	}
	return e.isExportedSymbol(node.Symbol())
}

func (e *extractor) isExportedSymbol(symbol *ast.Symbol) bool {
	if symbol == nil {
		return false
	}
	if symbol.ExportSymbol() != nil {
		return true
	}
	exportSymbol := e.checker.GetExportSymbolOfSymbol(symbol)
	return exportSymbol != nil && exportSymbol != symbol
}

func (e *extractor) functionReturnTypeText(node *ast.Node) string {
	signature := e.checker.GetSignatureFromDeclaration(node)
	if signature == nil {
		return ""
	}
	return e.renderTypeText(e.checker.GetReturnTypeOfSignature(signature), node)
}

func (e *extractor) functionParameterTypeTexts(node *ast.Node) []string {
	parameters := node.Parameters()
	texts := make([]string, 0, len(parameters))
	for _, parameter := range parameters {
		if parameter == nil {
			texts = append(texts, "")
			continue
		}
		texts = append(texts, e.renderTypeText(e.checker.GetTypeAtLocation(parameter.AsNode()), parameter.AsNode()))
	}
	return texts
}

func (e *extractor) renderTypeText(t *checker.Type, enclosing *ast.Node) string {
	if t == nil {
		return ""
	}
	return e.checker.TypeToStringEx(
		t,
		enclosing,
		checker.TypeFormatFlagsNoTruncation|checker.TypeFormatFlagsUseAliasDefinedOutsideCurrentScope,
		nil,
	)
}

// extractImportedSymbols returns the bindings that an import declaration,
// a re-export or an import-equals declaration introduces. For a re-export,
// the names are the names that the target module exports.
func extractImportedSymbols(node *ast.Node, isTypeOnly bool) ([]ImportedSymbol, bool, bool, bool) {
	switch {
	case ast.IsExportDeclaration(node):
		return extractReexportedSymbols(node, isTypeOnly)
	case ast.IsImportEqualsDeclaration(node):
		return []ImportedSymbol{{Name: node.Name().Text(), Kind: "namespace", IsTypeOnly: isTypeOnly}}, false, true, false
	case !ast.IsImportDeclaration(node):
		return nil, false, false, false
	}
	decl := node.AsImportDeclaration()
	if decl.ImportClause == nil {
		return nil, false, false, false
	}
	clause := decl.ImportClause.AsImportClause()
	if clause == nil {
		return nil, false, false, false
	}

	symbols := make([]ImportedSymbol, 0, 4)
	hasDefaultImport := false
	hasNamespaceImport := false
	hasNamedImports := false

	if name := clause.Name(); name != nil {
		hasDefaultImport = true
		symbols = append(symbols, ImportedSymbol{
			Name:       name.Text(),
			Kind:       "default",
			IsTypeOnly: isTypeOnly,
		})
	}
	if clause.NamedBindings == nil {
		return symbols, hasDefaultImport, hasNamespaceImport, hasNamedImports
	}

	switch clause.NamedBindings.Kind {
	case ast.KindNamespaceImport:
		hasNamespaceImport = true
		if name := clause.NamedBindings.Name(); name != nil {
			symbols = append(symbols, ImportedSymbol{
				Name:       name.Text(),
				Kind:       "namespace",
				IsTypeOnly: clause.NamedBindings.IsTypeOnly(),
			})
		}
	case ast.KindNamedImports:
		hasNamedImports = true
		for _, specifier := range clause.NamedBindings.Elements() {
			if specifier == nil || specifier.Name() == nil {
				continue
			}
			symbols = append(symbols, ImportedSymbol{
				Name:       specifier.Name().Text(),
				Kind:       "named",
				IsTypeOnly: specifier.IsTypeOnly() || isTypeOnly,
			})
		}
	}
	return symbols, hasDefaultImport, hasNamespaceImport, hasNamedImports
}

func extractReexportedSymbols(node *ast.Node, isTypeOnly bool) ([]ImportedSymbol, bool, bool, bool) {
	clause := node.AsExportDeclaration().ExportClause
	if clause == nil {
		return nil, false, false, false
	}
	if clause.Kind == ast.KindNamespaceExport {
		return []ImportedSymbol{{Name: clause.Name().Text(), Kind: "namespace", IsTypeOnly: isTypeOnly}}, false, true, false
	}
	symbols := make([]ImportedSymbol, 0, 4)
	for _, specifier := range clause.Elements() {
		if specifier == nil || specifier.Name() == nil {
			continue
		}
		nameNode := specifier.Name()
		if property := specifier.PropertyName(); property != nil {
			nameNode = property
		}
		symbols = append(symbols, ImportedSymbol{
			Name:       nameNode.Text(),
			Kind:       "named",
			IsTypeOnly: specifier.IsTypeOnly() || isTypeOnly,
		})
	}
	return symbols, false, false, len(symbols) > 0
}

func (e *extractor) resolveTypeTargetPath(nameNode *ast.Node) string {
	seen := map[*ast.Symbol]struct{}{}
	queue := make([]*ast.Symbol, 0, 4)
	enqueue := func(symbol *ast.Symbol) {
		if symbol == nil {
			return
		}
		if _, ok := seen[symbol]; ok {
			return
		}
		seen[symbol] = struct{}{}
		queue = append(queue, symbol)
	}

	e.enqueueTypeReferenceSymbols(nameNode, enqueue)
	if typ := e.checker.GetTypeAtLocation(nameNode); typ != nil {
		enqueue(typ.Symbol())
	}

	for len(queue) > 0 {
		symbol := queue[0]
		queue = queue[1:]

		if symbol.Flags()&ast.SymbolFlagsAlias != 0 {
			if aliased := e.checker.GetAliasedSymbol(symbol); aliased != nil && aliased != symbol {
				if path := e.symbolTargetPath(aliased); path != "" {
					return path
				}
				enqueue(aliased)
			}
			if immediate := e.checker.GetImmediateAliasedSymbol(symbol); immediate != nil && immediate != symbol {
				if path := e.symbolTargetPath(immediate); path != "" {
					return path
				}
				enqueue(immediate)
			}
		}
		exportSymbol := e.checker.GetExportSymbolOfSymbol(symbol)
		if exportSymbol != nil && exportSymbol != symbol {
			enqueue(exportSymbol)
		}
		if declaredType := e.checker.GetDeclaredTypeOfSymbol(symbol); declaredType != nil {
			enqueue(declaredType.Symbol())
		}
		if path := e.symbolTargetPath(symbol); path != "" {
			return path
		}
	}

	return ""
}

func (e *extractor) enqueueTypeReferenceSymbols(nameNode *ast.Node, enqueue func(*ast.Symbol)) {
	if nameNode == nil {
		return
	}

	enqueue(e.checker.GetSymbolAtLocation(nameNode))

	switch {
	case ast.IsIdentifier(nameNode), ast.IsPrivateIdentifier(nameNode):
		enqueue(e.checker.GetResolvedSymbol(nameNode))
	case ast.IsQualifiedName(nameNode):
		right := nameNode.AsQualifiedName().Right.AsNode()
		enqueue(e.checker.GetSymbolAtLocation(right))
		enqueue(e.checker.GetResolvedSymbol(right))
	case ast.IsPropertyAccessExpression(nameNode):
		right := nameNode.AsPropertyAccessExpression().Name().AsNode()
		enqueue(e.checker.GetSymbolAtLocation(right))
		enqueue(e.checker.GetResolvedSymbol(right))
	}
}

func (e *extractor) symbolTargetPath(symbol *ast.Symbol) string {
	if symbol == nil {
		return ""
	}
	if path := e.nodeTargetPath(symbol.ValueDeclaration()); path != "" {
		return path
	}
	for _, decl := range symbol.Declarations() {
		if path := e.nodeTargetPath(decl); path != "" {
			return path
		}
	}
	return ""
}

func (e *extractor) nodeTargetPath(node *ast.Node) string {
	if node == nil {
		return ""
	}
	file := ast.GetSourceFileOfNode(node)
	if file == nil {
		return ""
	}
	fileName := normalizeSlash(file.FileName().AsString())
	if !isWithinRoot(e.workspaceRoot, fileName) {
		return ""
	}
	return e.relativePath(fileName)
}

func (e *extractor) moduleRange(file *ast.SourceFile) SourceLocation {
	return e.location(file, 0, len(file.Text()))
}

// nodeLocation returns the range of a node from its first token, without
// the leading trivia.
func (e *extractor) nodeLocation(file *ast.SourceFile, node *ast.Node) SourceLocation {
	return e.location(file, scanner.GetTokenPosOfNode(node, file, false), node.End())
}

func (e *extractor) location(file *ast.SourceFile, start int, end int) SourceLocation {
	relative := e.relativePath(file.FileName().AsString())
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
			if !ast.HasSyntacticModifier(node, ast.ModifierFlagsDefault) {
				return nil
			}
			return &functionMeta{
				localName:    "default",
				kind:         "function",
				nameNode:     node,
				lookupNodes:  []*ast.Node{node},
				namingSource: node,
				exported:     true,
			}
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
			localName:    propertyNameText(nameNode),
			kind:         "method",
			nameNode:     nameNode,
			lookupNodes:  []*ast.Node{node, nameNode},
			namingSource: node,
		}
	case ast.IsGetAccessorDeclaration(node), ast.IsSetAccessorDeclaration(node):
		name := node.Name()
		if name == nil {
			return nil
		}
		kind := "get_accessor"
		if ast.IsSetAccessorDeclaration(node) {
			kind = "set_accessor"
		}
		nameNode := name.AsNode()
		return &functionMeta{
			localName:    propertyNameText(nameNode),
			kind:         kind,
			nameNode:     nameNode,
			lookupNodes:  []*ast.Node{node},
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
			localName:    propertyNameText(nameNode),
			kind:         kind,
			nameNode:     nameNode,
			namingSource: parent,
			lookupNodes:  []*ast.Node{node, parent, nameNode},
		}
	}
	if ast.IsPropertyDeclaration(parent) && parent.Initializer() == node && parent.Name() != nil {
		nameNode := parent.Name().AsNode()
		return &functionMeta{
			localName:    propertyNameText(nameNode),
			kind:         kind,
			nameNode:     nameNode,
			namingSource: parent,
			lookupNodes:  []*ast.Node{node, parent, nameNode},
		}
	}
	if ast.IsExportAssignment(parent) && !parent.AsExportAssignment().IsExportEquals && node.Name() == nil {
		return &functionMeta{
			localName:    "default",
			kind:         kind,
			nameNode:     node,
			namingSource: parent,
			lookupNodes:  []*ast.Node{node, parent},
			exported:     true,
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
				parts = append([]string{propertyNameText(parent.Name().AsNode())}, parts...)
			}
		case ast.IsConstructorDeclaration(parent):
			parts = append([]string{"constructor"}, parts...)
		case ast.IsVariableDeclaration(parent):
			if parent.Name() != nil && parent.Initializer() == child && (ast.IsObjectLiteralExpression(child) || child.Kind == ast.KindClassExpression) {
				parts = append([]string{parent.Name().Text()}, parts...)
			}
		case ast.IsPropertyAssignment(parent):
			if parent.Name() != nil && parent.Initializer() == child && (ast.IsObjectLiteralExpression(child) || child.Kind == ast.KindClassExpression) {
				parts = append([]string{propertyNameText(parent.Name().AsNode())}, parts...)
			}
		}
	}
	return strings.Join(parts, ".")
}

// propertyNameText returns the text of a property name. A computed name that
// is not a literal, such as [Symbol.iterator], gives its source text in
// brackets.
func propertyNameText(name *ast.Node) string {
	if text, ok := ast.TryGetTextOfPropertyName(name); ok {
		return text
	}
	return "[" + scanner.GetTextOfNode(name.Expression()) + "]"
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
	return fmt.Sprintf("%s:%d:%d:%d", normalizeSlash(file.FileName().AsString()), node.Kind, node.Pos(), node.End())
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
