package lintaiapi

import (
	"fmt"
	"slices"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/scanner"
)

const (
	AmbientFormReference = "reference"
	AmbientFormCall      = "call"
	AmbientFormConstruct = "construct"
)

type AmbientModuleBinding struct {
	Specifier string
	Export    string
	Path      []string
}

type AmbientMatcher struct {
	Source     string
	Capability string
	Root       string
	Path       []string
	Modules    []AmbientModuleBinding
	Form       string
	ArgCount   *int
	KeyMember  bool
}

type AmbientUse struct {
	EntityID            string
	SemanticKey         string
	Source              string
	Capability          string
	FilePath            string
	Key                 string
	HasKey              bool
	Dynamic             bool
	Write               bool
	FunctionSemanticKey string
	Range               SourceLocation
}

type ambientIndex struct {
	matchers    []*AmbientMatcher
	roots       map[string]struct{}
	specifiers  map[string]struct{}
	memberNames map[string]struct{}
}

// GlobalRef is a reference to a global value that the environment declares:
// a binding from the default library, from @types packages or from a
// workspace declaration file. Local bindings with the same name are not
// global references.
type GlobalRef struct {
	EntityID            string
	SemanticKey         string
	Name                string
	FilePath            string
	FunctionSemanticKey string
	Range               SourceLocation
}

type ambientFileState struct {
	file          *ast.SourceFile
	relative      string
	importedNames map[string]struct{}
}

type ambientSite struct {
	node    *ast.Node
	key     string
	hasKey  bool
	dynamic bool
	write   bool
}

func newAmbientIndex(matchers []AmbientMatcher) *ambientIndex {
	if len(matchers) == 0 {
		return nil
	}
	index := &ambientIndex{
		roots:       make(map[string]struct{}),
		specifiers:  make(map[string]struct{}),
		memberNames: make(map[string]struct{}),
	}
	for i := range matchers {
		matcher := &matchers[i]
		index.matchers = append(index.matchers, matcher)
		index.roots[matcher.Root] = struct{}{}
		if len(matcher.Path) > 0 {
			index.memberNames[matcher.Path[len(matcher.Path)-1]] = struct{}{}
		}
		for _, binding := range matcher.Modules {
			index.specifiers[binding.Specifier] = struct{}{}
			if binding.Export != "default" && binding.Export != "*" {
				index.memberNames[binding.Export] = struct{}{}
			}
		}
	}
	slices.SortStableFunc(index.matchers, func(a, b *AmbientMatcher) int {
		return len(b.Path) - len(a.Path)
	})
	return index
}

func (e *extractor) beginAmbientFile(file *ast.SourceFile) {
	e.ambientFile = nil
	if file.IsDeclarationFile {
		return
	}
	state := &ambientFileState{
		file:          file,
		relative:      e.relativePath(normalizeSlash(file.FileName().AsString())),
		importedNames: make(map[string]struct{}),
	}
	e.ambientFile = state
	if e.ambient == nil {
		return
	}
	for _, statement := range file.Statements.Nodes {
		if !ast.IsImportDeclaration(statement) {
			continue
		}
		specifier := statement.ModuleSpecifier()
		if specifier == nil || !ast.IsStringLiteralLike(specifier) {
			continue
		}
		if _, ok := e.ambient.specifiers[specifier.Text()]; !ok {
			continue
		}
		clause := statement.AsImportDeclaration().ImportClause
		if clause == nil {
			continue
		}
		if name := clause.Name(); name != nil {
			state.importedNames[name.Text()] = struct{}{}
		}
		bindings := clause.AsImportClause().NamedBindings
		if bindings == nil {
			continue
		}
		switch bindings.Kind {
		case ast.KindNamespaceImport:
			if name := bindings.Name(); name != nil {
				state.importedNames[name.Text()] = struct{}{}
			}
		case ast.KindNamedImports:
			for _, element := range bindings.Elements() {
				if element != nil && element.Name() != nil {
					state.importedNames[element.Name().Text()] = struct{}{}
				}
			}
		}
	}
}

func (e *extractor) matchAmbient(node *ast.Node, currentFunction *functionState) {
	state := e.ambientFile
	if state == nil || e.ambient == nil || !ast.IsIdentifier(node) || !isAmbientCandidatePosition(node) {
		return
	}
	text := node.Text()
	_, isRoot := e.ambient.roots[text]
	_, isImported := state.importedNames[text]
	isGlobalObject := text == "globalThis" || text == "window"
	if !isRoot && !isImported && !isGlobalObject {
		return
	}

	var symbol *ast.Symbol
	if ast.IsShorthandPropertyAssignment(node.Parent) {
		symbol = e.checker.GetShorthandAssignmentValueSymbol(node.Parent)
	} else {
		symbol = e.checker.GetSymbolAtLocation(node)
	}
	global := isGlobalSymbol(symbol)

	for _, matcher := range e.ambient.matchers {
		start, startPath, ok := ambientStart(node, text, symbol, global, matcher)
		if !ok {
			continue
		}
		full, ok := walkAmbientPath(start, startPath, matcher.Path)
		if !ok || !ambientFormMatches(full, matcher) {
			continue
		}
		if _, done := e.ambientDone[full]; done {
			return
		}
		e.recordAmbient(state, matcher, full, currentFunction)
		return
	}
}

// matchAmbientMember matches a member access, a destructured property or a
// destructuring-assignment property by the symbol that the checker resolves
// for it. This finds uses through aliases, for example const p = process;
// p.env.X, const D = Date; D.now() and const { now } = Date.
func (e *extractor) matchAmbientMember(node *ast.Node, currentFunction *functionState) {
	state := e.ambientFile
	if state == nil || e.ambient == nil {
		return
	}
	var symbol *ast.Symbol
	switch {
	case ast.IsPropertyAccessExpression(node):
		if !e.ambient.hasMemberName(node.Name().Text()) {
			return
		}
		symbol = e.checker.GetSymbolAtLocation(node.Name())
	case ast.IsElementAccessExpression(node):
		argument := ast.SkipParentheses(node.AsElementAccessExpression().ArgumentExpression)
		if argument == nil || !ast.IsStringLiteralLike(argument) || !e.ambient.hasMemberName(argument.Text()) {
			return
		}
		symbol = e.checker.GetSymbolAtLocation(argument)
	case ast.IsBindingElement(node):
		pattern := node.Parent
		if pattern == nil || !ast.IsObjectBindingPattern(pattern) || node.AsBindingElement().DotDotDotToken != nil {
			return
		}
		nameNode := node.PropertyName()
		if nameNode == nil {
			nameNode = node.Name()
		}
		name := propertyNameText(nameNode)
		if name == "" || !e.ambient.hasMemberName(name) {
			return
		}
		patternType := e.checker.GetTypeAtLocation(pattern)
		if patternType == nil {
			return
		}
		symbol = e.checker.GetPropertyOfType(patternType, name)
	case ast.IsShorthandPropertyAssignment(node), ast.IsPropertyAssignment(node):
		literal := node.Parent
		nameNode := node.Name()
		if literal == nil || nameNode == nil || !ast.IsIdentifier(nameNode) || !e.ambient.hasMemberName(nameNode.Text()) || !ast.IsArrayLiteralOrObjectLiteralDestructuringPattern(literal) {
			return
		}
		symbol = e.checker.GetPropertySymbolOfDestructuringAssignment(nameNode)
	default:
		return
	}
	if symbol == nil {
		return
	}
	if _, done := e.ambientDone[node]; done {
		return
	}
	for _, matcher := range e.ambientMemberMatchers()[e.skipAlias(symbol)] {
		if !ambientFormMatches(node, matcher) {
			continue
		}
		e.recordAmbient(state, matcher, node, currentFunction)
		return
	}
}

// ambientMemberMatchers maps the symbol of the last member of each matcher
// path to the matchers, for example the symbol of Process.env to the env
// matcher. The checker resolves the symbols once, from the global root and
// from each module binding of the matcher. The root symbol itself is not in
// the map: matchAmbient matches identifiers that refer to the root.
func (e *extractor) ambientMemberMatchers() map[*ast.Symbol][]*AmbientMatcher {
	if e.ambientMembers != nil {
		return e.ambientMembers
	}
	e.ambientMembers = make(map[*ast.Symbol][]*AmbientMatcher)
	add := func(symbol *ast.Symbol, matcher *AmbientMatcher) {
		if symbol == nil {
			return
		}
		symbol = e.skipAlias(symbol)
		if !slices.Contains(e.ambientMembers[symbol], matcher) {
			e.ambientMembers[symbol] = append(e.ambientMembers[symbol], matcher)
		}
	}
	for _, matcher := range e.ambient.matchers {
		if len(matcher.Path) > 0 {
			add(e.walkMemberPath(e.globalSymbol(matcher.Root), matcher.Path), matcher)
		}
		for _, binding := range matcher.Modules {
			if len(binding.Path) > len(matcher.Path) || !slices.Equal(binding.Path, matcher.Path[:len(binding.Path)]) {
				continue
			}
			start := e.moduleExportSymbol(binding.Specifier, binding.Export)
			rest := matcher.Path[len(binding.Path):]
			if len(rest) == 0 && (binding.Export == "default" || binding.Export == "*") {
				continue
			}
			add(e.walkMemberPath(start, rest), matcher)
		}
	}
	return e.ambientMembers
}

func (index *ambientIndex) hasMemberName(name string) bool {
	_, ok := index.memberNames[name]
	return ok
}

func (e *extractor) walkMemberPath(symbol *ast.Symbol, path []string) *ast.Symbol {
	for _, step := range path {
		if symbol == nil {
			return nil
		}
		symbolType := e.checker.GetTypeOfSymbol(e.skipAlias(symbol))
		if symbolType == nil {
			return nil
		}
		symbol = e.checker.GetPropertyOfType(symbolType, step)
	}
	return symbol
}

// moduleExportSymbol returns the symbol of an export of a declared module,
// for example randomUUID of "node:crypto". "*" is the module itself. "default"
// is the default export, the value of export =, or the module itself when it
// has neither, as with an esModuleInterop default import.
func (e *extractor) moduleExportSymbol(specifier string, export string) *ast.Symbol {
	moduleSymbol := e.checker.TryFindAmbientModule(specifier)
	if moduleSymbol == nil {
		return nil
	}
	resolved := e.checker.ResolveExternalModuleSymbol(moduleSymbol)
	if resolved == nil {
		resolved = moduleSymbol
	}
	if export == "*" {
		return resolved
	}
	if symbol := e.checker.TryGetMemberInModuleExports(export, moduleSymbol); symbol != nil {
		return symbol
	}
	if export == "default" {
		return resolved
	}
	return nil
}

func (e *extractor) skipAlias(symbol *ast.Symbol) *ast.Symbol {
	if symbol != nil && symbol.Flags()&ast.SymbolFlagsAlias != 0 {
		return e.checker.SkipAlias(symbol)
	}
	return symbol
}

// globalSymbol returns the global value symbol with the name, or nil.
func (e *extractor) globalSymbol(name string) *ast.Symbol {
	if symbol, ok := e.globalSyms[name]; ok {
		return symbol
	}
	symbol := e.checker.GetGlobalSymbol(name, ast.SymbolFlagsValue, nil)
	if symbol != nil {
		symbol = e.checker.GetMergedSymbol(symbol)
		if !isGlobalSymbol(symbol) {
			symbol = nil
		}
	}
	e.globalSyms[name] = symbol
	return symbol
}

var globalObjectNames = map[string]struct{}{"globalThis": {}, "window": {}, "self": {}, "global": {}}

// matchGlobalRef records a reference to a global value: an identifier that
// resolves to the global binding with its name, or a member of a global
// object, as in globalThis.crypto or window.fetch.
func (e *extractor) matchGlobalRef(node *ast.Node, currentFunction *functionState) {
	state := e.ambientFile
	if state == nil || !ast.IsIdentifier(node) || node.Parent == nil {
		return
	}
	name := node.Text()
	if name == "undefined" {
		return
	}
	global := e.globalSymbol(name)
	if global == nil {
		return
	}
	parent := node.Parent
	switch {
	case ast.IsPropertyAccessExpression(parent) && parent.Name() == node:
		object := ast.SkipParentheses(parent.Expression())
		if !ast.IsIdentifier(object) {
			return
		}
		if _, ok := globalObjectNames[object.Text()]; !ok {
			return
		}
		objectGlobal := e.globalSymbol(object.Text())
		if objectGlobal == nil || e.checker.GetMergedSymbol(e.checker.GetSymbolAtLocation(object)) != objectGlobal {
			return
		}
	case ast.IsShorthandPropertyAssignment(parent) && parent.Name() == node:
		if e.checker.GetMergedSymbol(e.checker.GetShorthandAssignmentValueSymbol(parent)) != global {
			return
		}
	default:
		if !isAmbientCandidatePosition(node) || e.checker.GetMergedSymbol(e.checker.GetSymbolAtLocation(node)) != global {
			return
		}
	}
	functionKey := ""
	if currentFunction != nil {
		functionKey = currentFunction.record.SemanticKey
	}
	baseKey := fmt.Sprintf("%s::global::%s", state.relative, name)
	semanticKey := withOrdinal(baseKey, nextOrdinal(e.globalRefCount, baseKey))
	start := scanner.GetTokenPosOfNode(node, state.file, false)
	e.globalRefs = append(e.globalRefs, GlobalRef{
		EntityID:            "global:" + semanticKey,
		SemanticKey:         semanticKey,
		Name:                name,
		FilePath:            state.relative,
		FunctionSemanticKey: functionKey,
		Range:               e.location(state.file, start, node.End()),
	})
}

// markAmbientDone marks the matched node and the member chain inside it, so
// that a shorter matcher does not record a second site for the same use.
func (e *extractor) markAmbientDone(full *ast.Node) {
	for node := full; node != nil; {
		e.ambientDone[node] = struct{}{}
		switch node.Kind {
		case ast.KindPropertyAccessExpression, ast.KindElementAccessExpression, ast.KindParenthesizedExpression, ast.KindNonNullExpression:
			node = node.Expression()
		default:
			node = nil
		}
	}
}

func ambientStart(node *ast.Node, text string, symbol *ast.Symbol, global bool, matcher *AmbientMatcher) (*ast.Node, []string, bool) {
	if global {
		if text == matcher.Root {
			return node, nil, true
		}
		if text == "globalThis" || text == "window" {
			if parent, name, ok := memberParent(node); ok && name == matcher.Root {
				return parent, nil, true
			}
		}
		return nil, nil, false
	}
	if len(matcher.Modules) == 0 {
		return nil, nil, false
	}
	specifier, export, ok := importBindingOf(symbol)
	if !ok {
		return nil, nil, false
	}
	start := node
	if export == "*" {
		parent, name, ok := memberParent(node)
		if !ok {
			return nil, nil, false
		}
		start, export = parent, name
	}
	for _, binding := range matcher.Modules {
		if binding.Specifier == specifier && binding.Export == export {
			return start, binding.Path, true
		}
	}
	return nil, nil, false
}

func (e *extractor) recordAmbient(state *ambientFileState, matcher *AmbientMatcher, full *ast.Node, currentFunction *functionState) {
	e.markAmbientDone(full)
	var sites []ambientSite
	if matcher.KeyMember {
		sites = ambientKeySites(full)
	} else {
		sites = []ambientSite{{node: full, write: isWriteTarget(full)}}
	}
	functionKey := ""
	if currentFunction != nil {
		functionKey = currentFunction.record.SemanticKey
	}
	for _, site := range sites {
		keyText := "*"
		if site.hasKey {
			keyText = site.key
		}
		baseKey := fmt.Sprintf("%s::ambient::%s.%s::%s", state.relative, matcher.Source, matcher.Capability, keyText)
		semanticKey := withOrdinal(baseKey, nextOrdinal(e.ambientCount, baseKey))
		start := scanner.GetTokenPosOfNode(site.node, state.file, false)
		e.ambientUses = append(e.ambientUses, AmbientUse{
			EntityID:            "ambient:" + semanticKey,
			SemanticKey:         semanticKey,
			Source:              matcher.Source,
			Capability:          matcher.Capability,
			FilePath:            state.relative,
			Key:                 site.key,
			HasKey:              site.hasKey,
			Dynamic:             site.dynamic,
			Write:               site.write,
			FunctionSemanticKey: functionKey,
			Range:               e.location(state.file, start, site.node.End()),
		})
	}
}

func isAmbientCandidatePosition(node *ast.Node) bool {
	parent := node.Parent
	if parent == nil {
		return false
	}
	if ast.IsShorthandPropertyAssignment(parent) && parent.Name() == node {
		return true
	}
	if ast.IsPropertyAccessExpression(parent) && parent.Name() == node {
		return false
	}
	if !ast.IsExpressionNode(node) {
		return false
	}
	return !ast.IsPartOfTypeQuery(node)
}

func isGlobalSymbol(symbol *ast.Symbol) bool {
	if symbol == nil {
		return true
	}
	for _, decl := range symbol.Declarations() {
		file := ast.GetSourceFileOfNode(decl)
		if file != nil && !file.IsDeclarationFile {
			return false
		}
	}
	return true
}

func importBindingOf(symbol *ast.Symbol) (string, string, bool) {
	if symbol == nil || symbol.Flags()&ast.SymbolFlagsAlias == 0 {
		return "", "", false
	}
	for _, decl := range symbol.Declarations() {
		var importDecl *ast.Node
		export := ""
		switch {
		case ast.IsImportSpecifier(decl):
			export = decl.Name().Text()
			if property := decl.PropertyName(); property != nil {
				export = property.Text()
			}
			importDecl = decl.Parent.Parent.Parent
		case ast.IsNamespaceImport(decl):
			export = "*"
			importDecl = decl.Parent.Parent
		case ast.IsImportClause(decl):
			export = "default"
			importDecl = decl.Parent
		default:
			continue
		}
		if importDecl == nil || !ast.IsImportDeclaration(importDecl) {
			continue
		}
		specifier := importDecl.ModuleSpecifier()
		if specifier == nil || !ast.IsStringLiteralLike(specifier) {
			continue
		}
		return specifier.Text(), export, true
	}
	return "", "", false
}

func outerTransparent(node *ast.Node) *ast.Node {
	for node.Parent != nil {
		parent := node.Parent
		if (parent.Kind == ast.KindParenthesizedExpression || parent.Kind == ast.KindNonNullExpression) && parent.Expression() == node {
			node = parent
			continue
		}
		break
	}
	return node
}

func memberParent(node *ast.Node) (*ast.Node, string, bool) {
	outer := outerTransparent(node)
	parent := outer.Parent
	if parent == nil {
		return nil, "", false
	}
	switch {
	case ast.IsPropertyAccessExpression(parent) && parent.Expression() == outer:
		return parent, parent.Name().Text(), true
	case ast.IsElementAccessExpression(parent) && parent.Expression() == outer:
		argument := ast.SkipParentheses(parent.AsElementAccessExpression().ArgumentExpression)
		if argument != nil && ast.IsStringLiteralLike(argument) {
			return parent, argument.Text(), true
		}
	}
	return nil, "", false
}

func walkAmbientPath(start *ast.Node, startPath []string, path []string) (*ast.Node, bool) {
	if len(startPath) > len(path) || !slices.Equal(startPath, path[:len(startPath)]) {
		return nil, false
	}
	node := start
	for _, step := range path[len(startPath):] {
		parent, name, ok := memberParent(node)
		if !ok || name != step {
			return nil, false
		}
		node = parent
	}
	return node, true
}

func ambientFormMatches(full *ast.Node, matcher *AmbientMatcher) bool {
	if matcher.Form == AmbientFormReference {
		return true
	}
	outer := outerTransparent(full)
	parent := outer.Parent
	if parent == nil {
		return false
	}
	switch matcher.Form {
	case AmbientFormCall:
		if !ast.IsCallExpression(parent) || parent.Expression() != outer {
			return false
		}
	case AmbientFormConstruct:
		if !ast.IsNewExpression(parent) || parent.Expression() != outer {
			return false
		}
	default:
		return false
	}
	return matcher.ArgCount == nil || len(parent.Arguments()) == *matcher.ArgCount
}

func ambientKeySites(full *ast.Node) []ambientSite {
	outer := outerTransparent(full)
	parent := outer.Parent
	switch {
	case parent == nil:
	case ast.IsPropertyAccessExpression(parent) && parent.Expression() == outer:
		return []ambientSite{{node: parent, key: parent.Name().Text(), hasKey: true, write: isWriteTarget(parent)}}
	case ast.IsElementAccessExpression(parent) && parent.Expression() == outer:
		argument := ast.SkipParentheses(parent.AsElementAccessExpression().ArgumentExpression)
		if argument != nil && ast.IsStringLiteralLike(argument) {
			return []ambientSite{{node: parent, key: argument.Text(), hasKey: true, write: isWriteTarget(parent)}}
		}
		return []ambientSite{{node: parent, dynamic: true, write: isWriteTarget(parent)}}
	case ast.IsVariableDeclaration(parent) && parent.Initializer() == outer && parent.Name() != nil && ast.IsObjectBindingPattern(parent.Name()):
		return bindingPatternSites(parent.Name())
	case ast.IsBinaryExpression(parent) && parent.AsBinaryExpression().Right == outer:
		binary := parent.AsBinaryExpression()
		switch binary.OperatorToken.Kind {
		case ast.KindEqualsToken:
			if left := ast.SkipParentheses(binary.Left); ast.IsObjectLiteralExpression(left) {
				return objectAssignmentSites(left)
			}
		case ast.KindInKeyword:
			if left := ast.SkipParentheses(binary.Left); ast.IsStringLiteralLike(left) {
				return []ambientSite{{node: parent, key: left.Text(), hasKey: true}}
			}
			return []ambientSite{{node: parent, dynamic: true}}
		}
	}
	return []ambientSite{{node: full, dynamic: true}}
}

func bindingPatternSites(pattern *ast.Node) []ambientSite {
	sites := make([]ambientSite, 0, len(pattern.Elements()))
	for _, element := range pattern.Elements() {
		if element == nil || !ast.IsBindingElement(element) {
			continue
		}
		binding := element.AsBindingElement()
		if binding.DotDotDotToken != nil {
			sites = append(sites, ambientSite{node: element, dynamic: true})
			continue
		}
		nameNode := binding.PropertyName
		if nameNode == nil {
			nameNode = element.Name()
		}
		sites = append(sites, propertyNameSite(element, nameNode))
	}
	return sites
}

func objectAssignmentSites(literal *ast.Node) []ambientSite {
	sites := make([]ambientSite, 0, len(literal.Properties()))
	for _, property := range literal.Properties() {
		switch {
		case ast.IsSpreadAssignment(property):
			sites = append(sites, ambientSite{node: property, dynamic: true})
		case ast.IsShorthandPropertyAssignment(property), ast.IsPropertyAssignment(property):
			sites = append(sites, propertyNameSite(property, property.Name()))
		}
	}
	return sites
}

func propertyNameSite(site *ast.Node, nameNode *ast.Node) ambientSite {
	if nameNode == nil {
		return ambientSite{node: site, dynamic: true}
	}
	if ast.IsComputedPropertyName(nameNode) {
		expression := ast.SkipParentheses(nameNode.Expression())
		if ast.IsStringLiteralLike(expression) {
			return ambientSite{node: site, key: expression.Text(), hasKey: true}
		}
		return ambientSite{node: site, dynamic: true}
	}
	if ast.IsIdentifier(nameNode) || ast.IsStringLiteralLike(nameNode) || ast.IsNumericLiteral(nameNode) {
		return ambientSite{node: site, key: nameNode.Text(), hasKey: true}
	}
	return ambientSite{node: site, dynamic: true}
}

func isWriteTarget(node *ast.Node) bool {
	outer := outerTransparent(node)
	parent := outer.Parent
	if parent == nil {
		return false
	}
	switch {
	case ast.IsBinaryExpression(parent):
		binary := parent.AsBinaryExpression()
		return binary.Left == outer && ast.IsAssignmentOperator(binary.OperatorToken.Kind)
	case ast.IsDeleteExpression(parent):
		return parent.Expression() == outer
	case ast.IsPrefixUnaryExpression(parent):
		operator := parent.AsPrefixUnaryExpression().Operator
		return operator == ast.KindPlusPlusToken || operator == ast.KindMinusMinusToken
	case ast.IsPostfixUnaryExpression(parent):
		return true
	}
	return false
}
