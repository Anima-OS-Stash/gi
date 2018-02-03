package compiler

import (
	"bytes"
	"os"
	//"encoding/json"
	"fmt"
	"github.com/gijit/gi/pkg/ast"
	//"github.com/gijit/gi/pkg/constant"
	"github.com/gijit/gi/pkg/printer"
	"github.com/gijit/gi/pkg/token"
	"github.com/gijit/gi/pkg/types"
	"sort"
	"strings"

	"github.com/gijit/gi/pkg/compiler/analysis"
	//"github.com/neelance/astrewrite"
	//"golang.org/x/tools/go/gcimporter15"
	"golang.org/x/tools/go/types/typeutil"
)

type pkgContext struct {
	*analysis.Info
	additionalSelections map[*ast.SelectorExpr]selection

	typeNames    []*types.TypeName
	pkgVars      map[string]string
	objectNames  map[types.Object]string
	varPtrNames  map[*types.Var]string
	anonTypes    []*types.TypeName
	anonTypeMap  typeutil.Map
	escapingVars map[*types.Var]bool
	indentation  int
	dependencies map[types.Object]bool
	minify       bool
	fileSet      *token.FileSet
	files        []*ast.File
	errList      ErrorList
}

func (p *pkgContext) SelectionOf(e *ast.SelectorExpr) (selection, bool) {
	if sel, ok := p.Selections[e]; ok {
		return sel, true
	}
	if sel, ok := p.additionalSelections[e]; ok {
		return sel, true
	}
	return nil, false
}

type selection interface {
	Kind() types.SelectionKind
	Recv() types.Type
	Index() []int
	Obj() types.Object
	Type() types.Type
}

type fakeSelection struct {
	kind  types.SelectionKind
	recv  types.Type
	index []int
	obj   types.Object
	typ   types.Type
}

func (sel *fakeSelection) Kind() types.SelectionKind { return sel.kind }
func (sel *fakeSelection) Recv() types.Type          { return sel.recv }
func (sel *fakeSelection) Index() []int              { return sel.index }
func (sel *fakeSelection) Obj() types.Object         { return sel.obj }
func (sel *fakeSelection) Type() types.Type          { return sel.typ }

type funcContext struct {
	*analysis.FuncInfo
	p             *pkgContext
	parent        *funcContext
	sig           *types.Signature
	allVars       map[string]int
	localVars     []string
	resultNames   []ast.Expr
	flowDatas     map[*types.Label]*flowData
	caseCounter   int
	labelCases    map[*types.Label]int
	output        []byte
	delayedOutput []byte
	posAvailable  bool
	pos           token.Pos

	genSymCounter int64

	intType types.Type
}

type flowData struct {
	postStmt  func()
	beginCase int
	endCase   int
}

type ImportContext struct {
	Packages map[string]*types.Package
	Import   func(string) (*Archive, error)
}

// packageImporter implements go/types.Importer interface.
type packageImporter struct {
	importContext *ImportContext
	importError   *error // A pointer to importError in Compile.
}

func (pi packageImporter) Import(path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}

	pp("pi = '%#v', pi.importContext='%#v'", pi, pi.importContext)
	pp("pi.importContext.Import='%#v'", pi.importContext.Import) // is nil!
	a, err := pi.importContext.Import(path)
	pp("jea debug: a *Archive back from pi.importContext.Import('%s') is '%#v'", path, a)
	if err != nil {
		if *pi.importError == nil {
			// If import failed, show first error of import only (https://github.com/gopherjs/gopherjs/issues/119).
			*pi.importError = err
		}
		return nil, err
	}

	tyPack := pi.importContext.Packages[a.ImportPath]

	// jea: import "fmt" gives not nil tyPack.
	pp("end of compiler.packageImporter.Import(), tyPack is '%#v'", tyPack)
	return tyPack, nil
}

func (c *funcContext) initArgs(ty types.Type) string {
	switch t := ty.(type) {
	case *types.Array:
		return fmt.Sprintf("%s, %d", c.typeName(t.Elem()), t.Len())
	case *types.Chan:
		return fmt.Sprintf("%s, %t, %t", c.typeName(t.Elem()), t.Dir()&types.SendOnly != 0, t.Dir()&types.RecvOnly != 0)
	case *types.Interface:
		methods := make([]string, t.NumMethods())
		for i := range methods {
			method := t.Method(i)
			pkgPath := ""
			if !method.Exported() {
				pkgPath = method.Pkg().Path()
			}
			methods[i] = fmt.Sprintf(`{prop: "%s", name: "%s", pkg: "%s", typ: $funcType(%s)}`, method.Name(), method.Name(), pkgPath, c.initArgs(method.Type()))
		}
		return fmt.Sprintf("[%s]", strings.Join(methods, ", "))
	case *types.Map:
		return fmt.Sprintf("%s, %s", c.typeName(t.Key()), c.typeName(t.Elem()))
	case *types.Pointer:
		return fmt.Sprintf("%s", c.typeName(t.Elem()))
	case *types.Slice:
		return fmt.Sprintf("%s", c.typeName(t.Elem()))
	case *types.Signature:
		params := make([]string, t.Params().Len())
		for i := range params {
			params[i] = c.typeName(t.Params().At(i).Type())
		}
		results := make([]string, t.Results().Len())
		for i := range results {
			results[i] = c.typeName(t.Results().At(i).Type())
		}
		return fmt.Sprintf("[%s], [%s], %t", strings.Join(params, ", "), strings.Join(results, ", "), t.Variadic())
	case *types.Struct:
		pkgPath := ""
		fields := make([]string, t.NumFields())
		for i := range fields {
			field := t.Field(i)
			if !field.Exported() {
				pkgPath = field.Pkg().Path()
			}
			fields[i] = fmt.Sprintf(`{prop: "%s", name: "%s", anonymous: %t, exported: %t, typ: %s, tag: %s}`, fieldName(t, i), field.Name(), field.Anonymous(), field.Exported(), c.typeName(field.Type()), encodeString(t.Tag(i)))
		}
		return fmt.Sprintf(`"%s", [%s]`, pkgPath, strings.Join(fields, ", "))
	default:
		panic("invalid type")
	}
}

func (c *funcContext) translateToplevelFunction(fun *ast.FuncDecl, info *analysis.FuncInfo) []byte {
	defer func() {
		pp("WHOPPER func done")
	}()
	o := c.p.Defs[fun.Name].(*types.Func)
	sig := o.Type().(*types.Signature)
	var recv *ast.Ident
	if fun.Recv != nil && fun.Recv.List[0].Names != nil {
		recv = fun.Recv.List[0].Names[0]
	}

	var joinedParams string
	primaryFunction := func(isMethod bool, funcRef string) []byte {
		if fun.Body == nil {
			return []byte(fmt.Sprintf("\t%s = function() \n\t\t$throwRuntimeError(\"native function not implemented: %s\");\n\t end ;\n", funcRef, o.FullName()))
		}

		params, fun := translateFunction(fun.Type, recv, fun.Body, c, sig, info, funcRef, isMethod)
		pp("funcRef in translateFunction, package.go:698 is '%s'; isMethod='%v'", funcRef, isMethod)
		splt := strings.Split(funcRef, ":")
		joinedParams = strings.Join(params, ", ")
		if isMethod {
			return []byte(fmt.Sprintf("\tfunction __type__%s%s;\n "+
				"__reg:AddMethod(\"struct\", \"%s\", \"%s\", __type__%s)\n",
				funcRef, fun,
				splt[0], splt[1], splt[0]+"."+splt[1],
			))
		} else {
			return []byte(fmt.Sprintf("\t%s = %s;\n", funcRef, fun))
		}
	}

	code := bytes.NewBuffer(nil)

	if fun.Recv == nil {
		funcRef := c.objectName(o)
		code.Write(primaryFunction(false, funcRef))
		if fun.Name.IsExported() {
			fmt.Fprintf(code, "\t%s = %s;\n", encodeIdent(fun.Name.Name), funcRef)
			//fmt.Fprintf(code, "\t$pkg.%s = %s;\n", encodeIdent(fun.Name.Name), funcRef)
		}
		return code.Bytes()
	}

	recvType := sig.Recv().Type()
	ptr, isPointer := recvType.(*types.Pointer)
	namedRecvType, _ := recvType.(*types.Named)
	if isPointer {
		namedRecvType = ptr.Elem().(*types.Named)
	}
	typeName := c.objectName(namedRecvType.Obj())
	funName := fun.Name.Name
	// jea
	//	if reservedKeywords[funName] {
	//		funName += "$"
	//	}

	if _, isStruct := namedRecvType.Underlying().(*types.Struct); isStruct {
		// jea
		// methods written here
		code.Write(primaryFunction(true, fmt.Sprintf("%s:%s", typeName, funName)))

		pp("WHOPPER! code is now '%s'", code.String())

		//code.Write(primaryFunction(false, typeName + ".ptr.prototype." + funName))

		//fmt.Fprintf(code, "\t%s.prototype.%s = function(%s) { return this.$val.%s(%s); };\n", typeName, funName, joinedParams, funName, joinedParams)
		return code.Bytes()
	}

	if isPointer {
		if _, isArray := ptr.Elem().Underlying().(*types.Array); isArray {
			code.Write(primaryFunction(false, typeName+".prototype."+funName))
			fmt.Fprintf(code, "\t$ptrType(%s).prototype.%s = function(%s) { return (new %s(this.$get())).%s(%s); };\n", typeName, funName, joinedParams, typeName, funName, joinedParams)
			return code.Bytes()
		}
		return primaryFunction(false, fmt.Sprintf("$ptrType(%s).prototype.%s", typeName, funName))
	}

	value := "this.$get()"
	if isWrapped(recvType) {
		value = fmt.Sprintf("new %s(%s)", typeName, value)
	}
	code.Write(primaryFunction(false, typeName+".prototype."+funName))
	fmt.Fprintf(code, "\t$ptrType(%s).prototype.%s = function(%s) { return %s.%s(%s); };\n", typeName, funName, joinedParams, value, funName, joinedParams)
	return code.Bytes()
}

func translateFunction(typ *ast.FuncType, recv *ast.Ident, body *ast.BlockStmt, outerContext *funcContext, sig *types.Signature, info *analysis.FuncInfo, funcRef string, isMethod bool) ([]string, string) {
	if info == nil {
		panic("nil info")
	}

	if false {
		// debug only
		pp("translateFunction called, body = ")
		printer.Fprint(os.Stdout, outerContext.p.fileSet, body)
	}

	c := &funcContext{
		FuncInfo:    info,
		p:           outerContext.p,
		parent:      outerContext,
		sig:         sig,
		allVars:     make(map[string]int, len(outerContext.allVars)),
		localVars:   []string{},
		flowDatas:   map[*types.Label]*flowData{nil: {}},
		caseCounter: 1,
		labelCases:  make(map[*types.Label]int),
	}
	for k, v := range outerContext.allVars {
		c.allVars[k] = v
	}
	prevEV := c.p.escapingVars

	var params []string
	for _, param := range typ.Params.List {
		if len(param.Names) == 0 {
			params = append(params, c.newVariable("param"))
			continue
		}
		for _, ident := range param.Names {
			if isBlank(ident) {
				params = append(params, c.newVariable("param"))
				continue
			}
			params = append(params, c.objectName(c.p.Defs[ident]))
		}
	}

	bodyOutput := string(c.CatchOutput(1, func() {
		if len(c.Blocking) != 0 {
			c.p.Scopes[body] = c.p.Scopes[typ]
			c.handleEscapingVars(body)
		}

		if c.sig != nil && c.sig.Results().Len() != 0 && c.sig.Results().At(0).Name() != "" {
			c.resultNames = make([]ast.Expr, c.sig.Results().Len())
			for i := 0; i < c.sig.Results().Len(); i++ {
				result := c.sig.Results().At(i)
				c.Printf("%s = %s;", c.objectName(result), c.translateExpr(c.zeroValue(result.Type()), nil).String())
				id := ast.NewIdent("")
				c.p.Uses[id] = result
				c.resultNames[i] = c.setType(id, result.Type())
			}
		}

		if recv != nil && !isBlank(recv) {
			this := "self"
			if isWrapped(c.p.TypeOf(recv)) {
				this = "this.$val"
			}
			c.Printf("%s = %s;", c.translateExpr(recv, nil), this)
		}

		c.translateStmtList(body.List)
		if len(c.Flattened) != 0 && !endsWithReturn(body.List) {
			c.translateStmt(&ast.ReturnStmt{}, nil)
		}
	}))

	sort.Strings(c.localVars)

	var prefix, suffix, functionName string

	if len(c.Flattened) != 0 {
		c.localVars = append(c.localVars, "$s")
		prefix = prefix + " $s = 0;"
	}

	if c.HasDefer {
		c.localVars = append(c.localVars, "$deferred")
		suffix = " }" + suffix
		if len(c.Blocking) != 0 {
			suffix = " }" + suffix
		}
	}

	if len(c.Blocking) != 0 {
		c.localVars = append(c.localVars, "$r")
		if funcRef == "" {
			funcRef = "$b"
			functionName = " $b"
		}
		var stores, loads string
		for _, v := range c.localVars {
			loads += fmt.Sprintf("%s = $f.%s; ", v, v)
			stores += fmt.Sprintf("$f.%s = %s; ", v, v)
		}
		prefix = prefix + " var $f, $c = false; if (this !== undefined && this.$blk !== undefined) { $f = this; $c = true; " + loads + "}"
		suffix = " if ($f === undefined) { $f = { $blk: " + funcRef + " }; } " + stores + "return $f;" + suffix
	}

	if c.HasDefer {
		prefix = prefix + " var $err = null; try {"
		deferSuffix := " } catch(err) { $err = err;"
		if len(c.Blocking) != 0 {
			deferSuffix += " $s = -1;"
		}
		if c.resultNames == nil && c.sig.Results().Len() > 0 {
			deferSuffix += fmt.Sprintf(" return%s;", c.translateResults(nil))
		}
		deferSuffix += " } finally { $callDeferred($deferred, $err);"
		if c.resultNames != nil {
			deferSuffix += fmt.Sprintf(" if (!$curGoroutine.asleep) { return %s; }", c.translateResults(c.resultNames))
		}
		if len(c.Blocking) != 0 {
			deferSuffix += " if($curGoroutine.asleep) {"
		}
		suffix = deferSuffix + suffix
	}

	if len(c.Flattened) != 0 {
		prefix = prefix + " s: while (true) { switch ($s) { case 0:"
		suffix = " } return; }" + suffix
	}

	formals := strings.Join(params, ", ")
	functionWord := "function"
	if isMethod {
		functionWord = ""
	}

	c.p.escapingVars = prevEV

	if c.HasDefer {
		pp("jea TODO: prefix is '%s'... should we not discard?", prefix)
		//		prefix = prefix + ...
		return params, fmt.Sprintf(`
%s%s(...) 
   local __orig = {...}
   local __defers={}
   local __zeroret = {}
   local __namedNames = {}
   local __actual=function(%s)
      %s
   end
   return __actuallyCall("%s", __actual, __namedNames, __zeroret, __defers, __orig)
end
`,
			functionWord, functionName, formals,
			bodyOutput, functionName)

		//prefix = prefix + " $deferred = []; $deferred.index = $curGoroutine.deferStack.length; $curGoroutine.deferStack.push($deferred);"
	}

	if prefix != "" {
		bodyOutput = strings.Repeat("\t", c.p.indentation+1) + "/*jea package.go:923 */" + prefix + "\n" + bodyOutput
	}
	if suffix != "" {
		bodyOutput = bodyOutput + strings.Repeat("\t", c.p.indentation+1) + "/*jea package.go:926 */" + suffix + "\n"
	}
	if len(c.localVars) != 0 {
		// jea, these are javascript only: at the top of a
		// function body, have vars a,b,c for the formal parameters f(a,b,c).
		//bodyOutput = fmt.Sprintf("%svar %s;\n", strings.Repeat("\t", c.p.indentation+1), strings.Join(c.localVars, ", ")) + bodyOutput
	}

	return params, fmt.Sprintf("%s%s(%s) \n%s%s end",
		functionWord, functionName, formals, bodyOutput, strings.Repeat("\t", c.p.indentation))
}
