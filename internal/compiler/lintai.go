package compiler

import (
	"github.com/microsoft/TypeScript/tsc/internal/module"
)

// NewModuleResolver returns a resolver with the program's options and host.
// lintaiapi uses it for specifiers that the program does not resolve, such
// as require() calls in TypeScript files.
func (p *Program) NewModuleResolver() *module.DefaultResolver {
	return p.newResolver()
}
