package indexer

import (
	"errors"
	"fmt"
	"go/ast"
	gotypes "go/types"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/matthewmueller/golly/types"
	"golang.org/x/tools/go/loader"
)

// Index struct
type Index struct {
	program      *loader.Program
	declarations map[string]*types.Declaration
	runtime      map[string]*types.Declaration
	imports      map[string]map[string]string
	interfaces   map[string]*gotypes.Interface
	receivers    map[string][]*receiver
	methods      map[string][]*types.Declaration
}

type receiver struct {
	Type     gotypes.Type
	Function *types.Declaration
}

// New maps all the declarations in all the packages
// this will be used as a lookup to map object declarations
// to actual AST nodes. object.String() is a unique identifier
// that points to a declaration in a go package (e.g. main())
func New(program *loader.Program) (*Index, error) {
	declarations := map[string]*types.Declaration{}
	interfaces := map[string]*gotypes.Interface{}
	runtime := map[string]*types.Declaration{}
	imports := map[string]map[string]string{}
	receivers := map[string][]*receiver{}

	runtimePath, err := getRuntimePath()
	if err != nil {
		return nil, err
	}

	// map[object.String()] => ast
	for _, info := range program.AllPackages {
		packagePath := info.Pkg.Path()
		for _, file := range info.Files {
			for _, decl := range file.Decls {
				switch t := decl.(type) {
				case *ast.FuncDecl:
					obj := info.ObjectOf(t.Name)
					name := t.Name.Name
					id := obj.String()

					var params []string
					for _, param := range t.Type.Params.List {
						for _, id := range param.Names {
							params = append(params, id.Name)
						}
					}

					// if it's a method don't export,
					// if it's the main() function
					// export either way
					exported := obj.Exported()
					if t.Recv != nil {
						exported = false
					} else if name == "main" {
						exported = true
					}

					declarations[id] = &types.Declaration{
						Exported: exported,
						From:     packagePath,
						Name:     name,
						ID:       id,
						Node:     decl,
						Params:   params,
					}

					// declaration may satisfy an interface
					// so we hold onto it for possible
					// inclusion later
					if t.Recv != nil {
						recv := t.Recv.List[0]
						if receivers[name] == nil {
							receivers[name] = []*receiver{}
						}
						receivers[name] = append(
							receivers[name],
							&receiver{
								Type:     info.TypeOf(recv.Type),
								Function: declarations[id],
							},
						)
					}

					// point human-friendly names to the declaration
					if runtimePath == packagePath {
						runtime[name] = declarations[id]
					}

				case *ast.GenDecl:
					for _, spec := range t.Specs {
						switch y := spec.(type) {
						case *ast.ValueSpec:
							for _, name := range y.Names {
								obj := info.ObjectOf(name)
								id := obj.String()

								declarations[id] = &types.Declaration{
									Exported: obj.Exported(),
									From:     packagePath,
									Name:     name.Name,
									ID:       id,
									Node:     decl,
								}
							}
						case *ast.TypeSpec:
							typeof := info.TypeOf(y.Name)
							obj := info.ObjectOf(y.Name)
							id := obj.String()

							declarations[id] = &types.Declaration{
								Exported: obj.Exported(),
								From:     packagePath,
								Name:     y.Name.Name,
								ID:       id,
								Node:     decl,
							}

							// alias to typeof
							// TODO: should this be the id we use?
							// it wouldn't work for valuespec, but
							// maybe here
							declarations[typeof.String()] = declarations[id]

							// store interface declarations for faster access
							if iface, ok := y.Type.(*ast.InterfaceType); ok {
								// never export an interface
								declarations[id].Exported = false

								ifacetype, ok := info.TypeOf(iface).(*gotypes.Interface)
								if !ok {
									return nil, fmt.Errorf("expected interface type to be gotypes.Interface")
								}
								// TODO: once again, not sure we need both here
								interfaces[id] = ifacetype
								interfaces[typeof.String()] = ifacetype
							}
						case *ast.ImportSpec:
							if imports[packagePath] == nil {
								imports[packagePath] = map[string]string{}
							}

							// trim the "" of package path's
							depPath := strings.Trim(y.Path.Value, `"`)

							// TODO: can y.Path be nil?
							var name string
							if y.Name != nil {
								name = y.Name.Name
							} else {
								name = path.Base(depPath)
							}

							imports[packagePath][name] = depPath
						}
					}
				default:
					return nil, fmt.Errorf("unhandled type %s", reflect.TypeOf(t))
				}
			}
		}
	}

	// rejiggle receivers to make accessing methods by receiver ID easier
	methods := map[string][]*types.Declaration{}
	for _, list := range receivers {
		for _, recv := range list {
			typeof := recv.Type.String()
			if methods[typeof] == nil {
				methods[typeof] = []*types.Declaration{}
			}
			methods[typeof] = append(methods[typeof], recv.Function)
		}
	}

	return &Index{
		program:      program,
		declarations: declarations,
		imports:      imports,
		runtime:      runtime,
		interfaces:   interfaces,
		receivers:    receivers,
		methods:      methods,
	}, nil
}

// FindByID finds a declaration from type object
func (i *Index) FindByID(id string) *types.Declaration {
	// ignore pointers (to support: p Person and p *Person)
	id = strings.TrimLeft(id, "*")
	return i.declarations[id]
}

// FindByObject finds a declaration from type object
func (i *Index) FindByObject(obj gotypes.Object) *types.Declaration {
	id := getDependency(obj)
	if id == "" {
		return nil
	}

	return i.FindByID(id)
}

// FindByIdent finds a declaration from an identifier
func (i *Index) FindByIdent(info *loader.PackageInfo, n *ast.Ident) *types.Declaration {
	obj := info.ObjectOf(n)
	if obj == nil {
		return nil
	}

	return i.FindByObject(obj)
}

// FindByNode finds a declaration from a node
func (i *Index) FindByNode(info *loader.PackageInfo, n ast.Node) *types.Declaration {
	switch t := n.(type) {
	case *ast.Ident:
		return i.FindByIdent(info, t)
	case *ast.StarExpr:
		return i.FindByNode(info, t.X)
	case *ast.SelectorExpr:
		return i.FindByNode(info, t.Sel)
	default:
		return nil
	}
}

// Imports returns the imports along with their aliases
func (i *Index) Imports(packagePath string) map[string]string {
	return i.imports[packagePath]
}

// Runtime returns a golly runtime declaration using it's name
func (i *Index) Runtime(name string) *types.Declaration {
	return i.runtime[name]
}

// ImplementedBy returns a list of declarations that the interface implements
func (i *Index) ImplementedBy(id string, method string) (decls []*types.Declaration) {
	iface := i.interfaces[id]
	if iface == nil {
		return decls
	}

	for _, recv := range i.receivers[method] {
		if gotypes.Implements(recv.Type, iface) {
			decls = append(decls, recv.Function)
		}
	}

	return decls
}

// Methods gets all the functions with receivers of a struct
func (i *Index) Methods(id string) (decls []*types.Declaration) {
	methods := i.methods[id]
	if methods != nil {
		return methods
	}
	methods = i.methods["*"+id]
	if methods != nil {
		return methods
	}
	return decls
}

func getDependency(obj gotypes.Object) string {
	if obj == nil {
		return ""
	}
	pkg := obj.Pkg()
	if pkg == nil {
		return ""
	}

	switch t := obj.(type) {
	case *gotypes.Var:
		return t.String()
	case *gotypes.Func:
		return t.String()
	case *gotypes.Const:
		return t.String()
	case *gotypes.TypeName:
		return t.String()
	}

	return ""
}

func getRuntimePath() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("unable to get the filepath")
	}
	runtimePkg, err := filepath.Rel(path.Join(os.Getenv("GOPATH"), "src"), path.Join(path.Dir(path.Dir(path.Dir(file))), "runtime"))
	if err != nil {
		return "", err
	}

	return runtimePkg, nil
}
