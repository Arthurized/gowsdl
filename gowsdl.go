// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package gowsdl

import (
	"bytes"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode"
)

const maxRecursion uint8 = 20

// GoWSDL defines the struct for WSDL generator.
type GoWSDL struct {
	loc                   *Location
	rawWSDL               []byte
	pkg                   string
	ignoreTLS             bool
	makePublicFn          func(string) string
	wsdl                  *WSDL
	resolvedXSDExternals  map[string]bool
	currentRecursionLevel uint8
	currentNamespace      string
}

// Method setNS sets (and returns) the currently active XML namespace.
func (g *GoWSDL) setNS(ns string) string {
	g.currentNamespace = ns
	return ns
}

// Method setNS returns the currently active XML namespace.
func (g *GoWSDL) getNS() string {
	return g.currentNamespace
}

var cacheDir = filepath.Join(os.TempDir(), "gowsdl-cache")

func init() {
	err := os.MkdirAll(cacheDir, 0700)
	if err != nil {
		log.Println("Create cache directory", "error", err)
		os.Exit(1)
	}
}

var timeout = time.Duration(30 * time.Second)

func dialTimeout(network, addr string) (net.Conn, error) {
	return net.DialTimeout(network, addr, timeout)
}

func downloadFile(url string, ignoreTLS bool) ([]byte, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: ignoreTLS,
		},
		Dial: dialTimeout,
	}
	client := &http.Client{Transport: tr}

	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Received response code %d", resp.StatusCode)
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// NewGoWSDL initializes WSDL generator.
func NewGoWSDL(file, pkg string, ignoreTLS bool, exportAllTypes bool) (*GoWSDL, error) {
	file = strings.TrimSpace(file)
	if file == "" {
		return nil, errors.New("WSDL file is required to generate Go proxy")
	}

	pkg = strings.TrimSpace(pkg)
	if pkg == "" {
		pkg = "myservice"
	}
	var makePublicFn func(string) string
	if exportAllTypes {
		makePublicFn = makePublic
	} else {
		makePublicFn = func(id string) string { return id }
	}

	loc, err := ParseLocation(file)
	if err != nil {
		return nil, err
	}

	wsdl := &GoWSDL{}
	wsdl.loc = loc
	wsdl.pkg = pkg
	wsdl.ignoreTLS = ignoreTLS
	wsdl.makePublicFn = makePublicFn
	return wsdl, nil
}

// Start initiaties the code generation process by starting two goroutines: one
// to generate types and another one to generate operations.
func (g *GoWSDL) Start() (map[string][]byte, error) {
	gocode := make(map[string][]byte)

	err := g.unmarshal()
	if err != nil {
		return nil, err
	}

	// Process WSDL nodes
	schemas := g.wsdl.Types.Schemas
	for i := 0; i < len(schemas); i++ {
		schema := schemas[i]
		t := newTraverser(schema, schemas)
		t.traverse()
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error

		gocode["types"], err = g.genTypes()
		if err != nil {
			log.Println("genTypes", "error", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error

		gocode["operations"], err = g.genOperations()
		if err != nil {
			log.Println(err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error

		gocode["server"], err = g.genServer()
		if err != nil {
			log.Println(err)
		}
	}()

	wg.Wait()

	gocode["header"], err = g.genHeader()
	if err != nil {
		log.Println(err)
	}

	gocode["server_header"], err = g.genServerHeader()
	if err != nil {
		log.Println(err)
	}

	gocode["server_wsdl"] = []byte("var wsdl = `" + string(g.rawWSDL) + "`")

	return gocode, nil
}

func (g *GoWSDL) fetchFile(loc *Location) (data []byte, err error) {
	if loc.f != "" {
		log.Println("Reading", "file", loc.f)
		data, err = ioutil.ReadFile(loc.f)
		return data, err
	}

	remote := loc.u.String()
	log.Println("Downloading", "file", remote)
	data, err = downloadFile(remote, g.ignoreTLS)
	return data, err
}

func (g *GoWSDL) unmarshal() error {
	data, err := g.fetchFile(g.loc)
	if err != nil {
		return err
	}

	parsed := new(WSDL)
	if err := xml.Unmarshal(data, parsed); err != nil {
		return err
	}
	g.wsdl = parsed
	g.rawWSDL = data

	for i := range g.wsdl.Types.Schemas {
		schema := g.wsdl.Types.Schemas[i]
		if err := g.resolveXSDExternals(schema, g.loc); err != nil {
			return err
		}
	}

	return nil
}

func (g *GoWSDL) resolveXSDExternals(schema *XSDSchema, loc *Location) error {
	download := func(base *Location, ref string) error {
		location, err := base.Parse(ref)
		if err != nil {
			return err
		}
		schemaKey := location.String()
		if g.resolvedXSDExternals[schemaKey] {
			return nil
		}
		if g.resolvedXSDExternals == nil {
			g.resolvedXSDExternals = make(map[string]bool, maxRecursion)
		}
		g.resolvedXSDExternals[schemaKey] = true

		data, err := g.fetchFile(location)
		if err != nil {
			return err
		}

		newschema := new(XSDSchema)
		if err := xml.Unmarshal(data, newschema); err != nil {
			return err
		}

		nested := len(newschema.Includes) > 0 || len(newschema.Imports) > 0
		if nested && g.currentRecursionLevel < maxRecursion {
			g.currentRecursionLevel++
			if err := g.resolveXSDExternals(newschema, location); err != nil {
				return err
			}
		}

		g.wsdl.Types.Schemas = append(g.wsdl.Types.Schemas, newschema)
		return nil
	}

	for _, impts := range schema.Imports {
		// Download the file only if we have a hint in the form of schemaLocation.
		if impts.SchemaLocation == "" {
			log.Printf("[WARN] Don't know where to find XSD for %s", impts.Namespace)
			continue
		}

		if e := download(loc, impts.SchemaLocation); e != nil {
			return e
		}
	}

	for _, incl := range schema.Includes {
		if e := download(loc, incl.SchemaLocation); e != nil {
			return e
		}
	}

	return nil
}

// renderTemplate parses text as a named template with the given helpers and
// executes it against data, returning the rendered bytes.
func renderTemplate(name, text string, funcs template.FuncMap, data interface{}) ([]byte, error) {
	var out bytes.Buffer
	tmpl := template.Must(template.New(name).Funcs(funcs).Parse(text))
	if err := tmpl.Execute(&out, data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (g *GoWSDL) genTypes() ([]byte, error) {
	funcMap := template.FuncMap{
		"toGoType":                 toGoType,
		"stripns":                  stripns,
		"replaceReservedWords":     replaceReservedWords,
		"replaceAttrReservedWords": replaceAttrReservedWords,
		"normalize":                normalize,
		"makePublic":               g.makePublicFn,
		"makeFieldPublic":          makePublic,
		"comment":                  comment,
		"removeNS":                 removeNS,
		"goString":                 goString,
		"findNameByType":           g.findNameByType,
		"removePointerFromType":    removePointerFromType,
		"setNS":                    g.setNS,
		"getNS":                    g.getNS,
	}

	return renderTemplate("types", typesTmpl, funcMap, g.wsdl.Types)
}

func (g *GoWSDL) genOperations() ([]byte, error) {
	funcMap := template.FuncMap{
		"toGoType":             toGoType,
		"stripns":              stripns,
		"replaceReservedWords": replaceReservedWords,
		"normalize":            normalize,
		"makePublic":           g.makePublicFn,
		"makePrivate":          makePrivate,
		"findType":             g.findType,
		"findSOAPAction":       g.findSOAPAction,
		"findServiceAddress":   g.findServiceAddress,
	}

	return renderTemplate("operations", opsTmpl, funcMap, g.wsdl.PortTypes)
}

func (g *GoWSDL) genServer() ([]byte, error) {
	funcMap := template.FuncMap{
		"toGoType":             toGoType,
		"stripns":              stripns,
		"replaceReservedWords": replaceReservedWords,
		"makePublic":           g.makePublicFn,
		"findType":             g.findType,
		"findSOAPAction":       g.findSOAPAction,
		"findServiceAddress":   g.findServiceAddress,
	}

	return renderTemplate("server", serverTmpl, funcMap, g.wsdl.PortTypes)
}

func (g *GoWSDL) genHeader() ([]byte, error) {
	funcMap := template.FuncMap{
		"toGoType":             toGoType,
		"stripns":              stripns,
		"replaceReservedWords": replaceReservedWords,
		"normalize":            normalize,
		"makePublic":           g.makePublicFn,
		"findType":             g.findType,
		"comment":              comment,
	}

	return renderTemplate("header", headerTmpl, funcMap, g.pkg)
}

func (g *GoWSDL) genServerHeader() ([]byte, error) {
	funcMap := template.FuncMap{
		"toGoType":             toGoType,
		"stripns":              stripns,
		"replaceReservedWords": replaceReservedWords,
		"makePublic":           g.makePublicFn,
		"findType":             g.findType,
		"comment":              comment,
	}

	return renderTemplate("server_header", serverHeaderTmpl, funcMap, g.pkg)
}

var reservedWords = map[string]string{
	"break":       "break_",
	"default":     "default_",
	"func":        "func_",
	"interface":   "interface_",
	"select":      "select_",
	"case":        "case_",
	"defer":       "defer_",
	"go":          "go_",
	"map":         "map_",
	"struct":      "struct_",
	"chan":        "chan_",
	"else":        "else_",
	"goto":        "goto_",
	"package":     "package_",
	"switch":      "switch_",
	"const":       "const_",
	"fallthrough": "fallthrough_",
	"if":          "if_",
	"range":       "range_",
	"type":        "type_",
	"continue":    "continue_",
	"for":         "for_",
	"import":      "import_",
	"return":      "return_",
	"var":         "var_",
}

var reservedWordsInAttr = map[string]string{
	"break":       "break_",
	"default":     "default_",
	"func":        "func_",
	"interface":   "interface_",
	"select":      "select_",
	"case":        "case_",
	"defer":       "defer_",
	"go":          "go_",
	"map":         "map_",
	"struct":      "struct_",
	"chan":        "chan_",
	"else":        "else_",
	"goto":        "goto_",
	"package":     "package_",
	"switch":      "switch_",
	"const":       "const_",
	"fallthrough": "fallthrough_",
	"if":          "if_",
	"range":       "range_",
	"type":        "type_",
	"continue":    "continue_",
	"for":         "for_",
	"import":      "import_",
	"return":      "return_",
	"var":         "var_",
	"string":      "astring",
}

var specialCharacterMapping = map[string]string{
	"+": "Plus",
	"@": "At",
}

// Replaces Go reserved keywords to avoid compilation issues
func replaceReservedWords(identifier string) string {
	if sanitized := reservedWords[identifier]; sanitized != "" {
		return sanitized
	}
	return normalize(identifier)
}

// Replaces Go reserved keywords to avoid compilation issues
func replaceAttrReservedWords(identifier string) string {
	if sanitized := reservedWordsInAttr[identifier]; sanitized != "" {
		return sanitized
	}
	return normalize(identifier)
}

// Normalizes value to be used as a valid Go identifier, avoiding compilation issues
func normalize(value string) string {
	v := value
	for k, rep := range specialCharacterMapping {
		if strings.Contains(v, k) {
			v = strings.ReplaceAll(v, k, rep)
		}
	}

	out := make([]rune, 0, len(v))
	for _, r := range v {
		if r == '.' || r == '-' {
			r = '_'
		}
		keep := false
		if r == '_' {
			keep = true
		} else if unicode.IsLetter(r) {
			keep = true
		} else if unicode.IsDigit(r) {
			keep = true
		}
		if keep {
			out = append(out, r)
		}
	}
	return string(out)
}

func goString(s string) string {
	escaped := s
	escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
	return escaped
}

var xsd2GoTypes = map[string]string{
	"string":             "string",
	"token":              "string",
	"float":              "float32",
	"double":             "float64",
	"decimal":            "float64",
	"integer":            "int32",
	"int":                "int32",
	"short":              "int16",
	"byte":               "int8",
	"long":               "int64",
	"boolean":            "bool",
	"datetime":           "soap.XSDDateTime",
	"date":               "soap.XSDDate",
	"time":               "soap.XSDTime",
	"base64binary":       "[]byte",
	"hexbinary":          "[]byte",
	"unsignedint":        "uint32",
	"nonnegativeinteger": "uint32",
	"unsignedshort":      "uint16",
	"unsignedbyte":       "byte",
	"unsignedlong":       "uint64",
	"anytype":            "AnyType",
	"ncname":             "NCName",
	"anyuri":             "AnyURI",
}

// localPart drops an optional "ns:" prefix from a qualified name
// (ie. xsd:string, xs:string) and returns the bare local name.
func localPart(qname string) string {
	parts := strings.Split(qname, ":")
	if len(parts) == 2 {
		return parts[1]
	}
	return parts[0]
}

func removeNS(xsdType string) string {
	return localPart(xsdType)
}

func toGoType(xsdType string, nillable bool) string {
	local := localPart(xsdType)
	key := strings.ToLower(local)

	mapped, ok := xsd2GoTypes[key]
	if ok && mapped != "" {
		goType := mapped
		if nillable {
			goType = "*" + goType
		}
		return goType
	}

	custom := makePublic(local)
	custom = replaceReservedWords(custom)
	return "*" + custom
}

func removePointerFromType(goType string) string {
	return regexp.MustCompile("^\\s*\\*").ReplaceAllLiteralString(goType, "")
}

// Given a message, finds its type.
//
// I'm not very proud of this function but
// it works for now and performance doesn't
// seem critical at this point
func (g *GoWSDL) findType(message string) string {
	message = stripns(message)

	for _, msg := range g.wsdl.Messages {
		if msg.Name != message {
			continue
		}

		// Assumes document/literal wrapped WS-I
		if len(msg.Parts) == 0 {
			// Message does not have parts. This could be a Port
			// with HTTP binding or SOAP 1.2 binding, which are not currently
			// supported.
			log.Printf("[WARN] %s message doesn't have any parts, ignoring message...", msg.Name)
			continue
		}

		part := msg.Parts[0]
		if part.Type != "" {
			return stripns(part.Type)
		}
		return g.resolveElementRef(stripns(part.Element))
	}
	return ""
}

// resolveElementRef maps a document/literal element reference to its Go type
// name, falling back to the element name, or "" when no element matches.
func (g *GoWSDL) resolveElementRef(ref string) string {
	for _, schema := range g.wsdl.Types.Schemas {
		for _, el := range schema.Elements {
			if !strings.EqualFold(ref, el.Name) {
				continue
			}
			if el.Type != "" {
				return stripns(el.Type)
			}
			return el.Name
		}
	}
	return ""
}

// Given a type, check if there's an Element with that type, and return its name.
func (g *GoWSDL) findNameByType(name string) string {
	return newTraverser(nil, g.wsdl.Types.Schemas).findNameByType(name)
}

// TODO(c4milo): Add support for namespaces instead of striping them out
// TODO(c4milo): improve runtime complexity if performance turns out to be an issue.
func (g *GoWSDL) findSOAPAction(operation, portType string) string {
	for _, binding := range g.wsdl.Binding {
		if !strings.EqualFold(stripns(binding.Type), portType) {
			continue
		}

		for _, soapOp := range binding.Operations {
			if soapOp.Name != operation {
				continue
			}
			return soapOp.SOAPOperation.SOAPAction
		}
	}
	return ""
}

func (g *GoWSDL) findServiceAddress(name string) string {
	for _, service := range g.wsdl.Service {
		for _, port := range service.Ports {
			if port.Name != name {
				continue
			}
			return port.SOAPAddress.Location
		}
	}
	return ""
}

// TODO(c4milo): Add namespace support instead of stripping it
func stripns(xsdType string) string {
	return localPart(xsdType)
}

func makePublic(identifier string) string {
	if isBasicType(identifier) {
		return identifier
	}
	runes := []rune(identifier)
	if len(runes) == 0 {
		return "EmptyString"
	}
	return string(unicode.ToUpper(runes[0])) + string(runes[1:])
}

var basicTypes = map[string]string{
	"string":      "string",
	"float32":     "float32",
	"float64":     "float64",
	"int":         "int",
	"int8":        "int8",
	"int16":       "int16",
	"int32":       "int32",
	"int64":       "int64",
	"bool":        "bool",
	"time.Time":   "time.Time",
	"[]byte":      "[]byte",
	"byte":        "byte",
	"uint16":      "uint16",
	"uint32":      "uint32",
	"uinit64":     "uint64",
	"interface{}": "interface{}",
}

func isBasicType(identifier string) bool {
	_, ok := basicTypes[identifier]
	return ok
}

func makePrivate(identifier string) string {
	runes := []rune(identifier)
	if len(runes) == 0 {
		return identifier
	}
	return string(unicode.ToLower(runes[0])) + string(runes[1:])
}

func comment(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) == 1 && lines[0] == "" {
		return ""
	}

	// Helps to determine if there is an actual comment without screwing newlines
	// in real comments.
	var output strings.Builder
	hasComment := false
	for _, line := range lines {
		line = strings.TrimLeftFunc(line, unicode.IsSpace)
		if line != "" {
			hasComment = true
		}
		output.WriteString("\n// " + line)
	}

	if !hasComment {
		return ""
	}
	return output.String()
}
