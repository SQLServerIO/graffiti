package tags

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"io/ioutil"
	"os"
	"strings"
	"text/template"

	"github.com/fatih/camelcase"
)

// Options contains the data needed to generate tags for a target (file or
// package).
type Options struct {
	// Target is the file or directory to parse, or current dir if empty.
	Target string
	// Tags contains schema names like json or yaml.
	Tags []string
	// Template is used to generate the contents of the struct tag field if Tags is empty.
	Template *template.Template
	// Mapping contains field name conversions. If a field isn't in the map, lowercase of field name is used.
	Mapping map[string]string
	// Types to generate tags for, if empty, all structs will have tags generated.
	Types []string
	// DryRun indicates whether we should simply write to StdOut rather than writing to the files.
	DryRun bool
	//Typescript indicates whether generate typescript class for structs or not
	Typescript bool
}

// Generate generates tags according to the given options.
func Generate(o Options) error {
	i, err := os.Stat(o.Target)
	if err != nil {
		return err
	}
	if !i.IsDir() {
		return genfile(o, o.Target)
	}

	p, err := build.Default.ImportDir(o.Target, 0)
	if err != nil {
		return err
	}
	for _, f := range p.GoFiles {
		if err := genfile(o, f); err != nil {
			return err
		}
	}
	return nil
}

// genfile generates struct tags for the given file.
func genfile(o Options, file string) error {
	fmt.Println("gen file", file)
	fset := token.NewFileSet()
	n, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		return err
	}
	c, err := ioutil.ReadFile(file)
	if err != nil {
		return err
	}
	v, err := walkAST(o, n, c)
	if err != nil {
		return err
	}
	b, err := gen(v, fset, n)
	if err != nil {
		return err
	}
	if b == nil {
		fmt.Println("nil bytes")
		// no changes
		return nil
	}
	var tsb []byte
	if o.Typescript {
		tsb = genTS(v.structs)
	}
	if o.DryRun {
		_, err := fmt.Fprintf(os.Stdout, "%s\n", b)
		_, err = fmt.Printf("%s\n", string(tsb))
		return err
	}

	err = ioutil.WriteFile(file, b, 0644)
	if err != nil {
		return err
	}
	if o.Typescript {
		return ioutil.WriteFile(file+".ts", tsb, 0644)
	}
	return nil
}

func walkAST(o Options, n ast.Node, raw []byte) (*visitor, error) {
	v := &visitor{Options: o, src: string(raw)}
	ast.Walk(v, n)
	if v.err != nil {
		return nil, v.err
	}
	if !v.changed {
		return nil, nil
	}
	return v, nil
}

// func gen(o Options, fset *token.FileSet, n ast.Node, raw []byte) ([]byte, error) {
func gen(v *visitor, fset *token.FileSet, n ast.Node) ([]byte, error) {
	// v := &visitor{Options: o, src: string(raw)}
	// ast.Walk(v, n)
	// if v.err != nil {
	// 	return nil, v.err
	// }
	// if !v.changed {
	// 	return nil, nil
	// }
	c := printer.Config{Mode: printer.RawFormat}
	buf := &bytes.Buffer{}
	if err := c.Fprint(buf, fset, n); err != nil {
		return nil, fmt.Errorf("error printing output: %s", err)
	}
	b, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, err
	}

	return b, nil
}

func genTS(structs []structDef) []byte {
	// fmt.Printf("structs is %#v", structs)
	fmt.Println("generating typescript...")
	tmpl := `
export class {{.Name}} {
{{range $i,$e := .Fields}}	{{.Name}}:{{if (eq $e.FieldType "float" "float32" "float64" "int" "uint" "int32" "uint32" "int64" "uint64")}}number;
{{else if eq $e.FieldType  "time.Time"}}Date;
{{else if eq $e.FieldType  "bool"}}boolean;
{{else if eq $e.FieldType  "string"}}string;
{{else}}any;
{{end}}{{end}}}
`
	buf := bytes.NewBufferString("")
	t := template.Must(template.New("ts").Parse(tmpl))
	for _, s := range structs {
		err := t.Execute(buf, s)
		if err != nil {
			fmt.Printf("tmplate render error %s \n", err.Error())
			return nil
		}
	}
	return buf.Bytes()
}

type structFieldDef struct {
	Name      string
	FieldType string
}
type structDef struct {
	Name   string
	Fields []structFieldDef
}

// visitor is a wrapper around Options that implement the ast.Visitor interface
// and some helper methods.  Since ast.Walk doesn't let you return values,
// we instead set the return values in this struct.
type visitor struct {
	Options
	// changed is true if the AST was changed by our code.
	changed bool
	// err is non-nil if there was an error processing the file.
	err error
	// src is the src file content
	src     string
	structs []structDef
}

// Visit implements ast.Visitor and does the meat of the tag generation.
func (v *visitor) Visit(n ast.Node) ast.Visitor {
	if v.structs == nil {
		v.structs = []structDef{}
	}

	if n == nil || v.err != nil {
		return nil
	}
	if t, ok := n.(*ast.TypeSpec); ok {
		if s, ok := t.Type.(*ast.StructType); ok {
			if !v.shouldGen(t.Name.Name) {
				return v
			}
			sd := structDef{Name: t.Name.Name, Fields: []structFieldDef{}}
			for _, f := range s.Fields.List {
				l := len(f.Names)
				if l > 1 || l <= 0 {
					// skip fields declared as a, b, c int
					// or embeded structs
					continue
				}
				name := f.Names[0].Name
				if !ast.IsExported(name) {
					// skip non-exported names
					continue
				}

				if f.Tag == nil {
					f.Tag = &ast.BasicLit{}
				}
				val, err := v.gen(name)
				if err != nil {
					v.err = err
					return nil
				}
				sfd := structFieldDef{}
				sfd.Name = toCamelCase(name)
				sfd.FieldType = v.src[f.Type.Pos()-1 : f.Type.End()-1]
				sd.Fields = append(sd.Fields, sfd)
				// fmt.Printf("original value %s \n", f.Tag.Value)
				// tag := ""
				// if f.Tag.Value != "" {
				// 	t := strings.TrimSuffix(f.Tag.Value, "`")
				// 	tag = t + " " + strings.TrimPrefix(val, "`")
				// } else {
				// 	tag = val
				// }
				// fmt.Printf("tag gen %s \n", tag)
				f.Tag.Value = val
				v.changed = true
			}
			v.structs = append(v.structs, sd)
		}

	}
	// fmt.Printf("fields composed done %#v\n", v.structs)
	return v
}

// shouldGen reports whether graffiti should generate tags for the struct with
// the given name.
func (v visitor) shouldGen(name string) bool {
	if len(v.Types) == 0 {
		return true
	}
	for _, typ := range v.Types {
		if typ == name {
			return true
		}
	}
	return false
}

// gen creates the struct tag for the given field name, according to the options
// set.
func (v visitor) gen(name string) (string, error) {
	if m, ok := v.Mapping[name]; ok {
		name = m
	} else {
		name = toCamelCase(name)
	}
	if len(v.Tags) > 0 {
		vals := make([]string, len(v.Tags))
		for i, t := range v.Tags {
			vals[i] = fmt.Sprintf("%s:%q", t, name)
		}
		return "`" + strings.Join(vals, " ") + "`", nil
	}

	// no tags means we have a template
	buf := &bytes.Buffer{}
	err := v.Template.Execute(buf, struct{ F string }{name})
	if err != nil {
		return "", err
	}
	return "`" + buf.String() + "`", nil
}

func toCamelCase(in string) string {
	ns := camelcase.Split(in)
	ns[0] = strings.ToLower(ns[0])
	return strings.Join(ns, "")
}
