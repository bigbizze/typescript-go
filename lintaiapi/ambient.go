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
	matchers   []*AmbientMatcher
	roots      map[string]struct{}
	specifiers map[string]struct{}
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
		roots:      make(map[string]struct{}),
		specifiers: make(map[string]struct{}),
	}
	for i := range matchers {
		matcher := &matchers[i]
		index.matchers = append(index.matchers, matcher)
		index.roots[matcher.Root] = struct{}{}
		for _, binding := range matcher.Modules {
			index.specifiers[binding.Specifier] = struct{}{}
		}
	}
	slices.SortStableFunc(index.matchers, func(a, b *AmbientMatcher) int {
		return len(b.Path) - len(a.Path)
	})
	return index
}

func (e *extractor) beginAmbientFile(file *ast.SourceFile) {
	e.ambientFile = nil
	if e.ambient == nil || file.IsDeclarationFile {
		return
	}
	state := &ambientFileState{
		file:          file,
		relative:      e.relativePath(normalizeSlash(file.FileName().AsString())),
		importedNames: make(map[string]struct{}),
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
	e.ambientFile = state
}

func (e *extractor) matchAmbient(node *ast.Node, currentFunction *functionState) {
	state := e.ambientFile
	if state == nil || !ast.IsIdentifier(node) || !isAmbientCandidatePosition(node) {
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
		e.recordAmbient(state, matcher, full, currentFunction)
		return
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

func mergeAmbientUses(existing []AmbientUse, incoming []AmbientUse) []AmbientUse {
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
