package gowsdl

import (
	"encoding/xml"
	"strings"
)

type traverseMode int32

const (
	refResolution traverseMode = iota
	findNameByType
)

type traverser struct {
	c   *XSDSchema
	all []*XSDSchema
	tm  traverseMode
	// fields used by findNameByType mode
	typeName             string
	foundElmName         string
	conflictingTypeUsage bool
}

func newTraverser(c *XSDSchema, all []*XSDSchema) *traverser {
	t := &traverser{c: c, all: all}
	t.tm = refResolution // default traverse mode is refResolution
	return t
}

// walkComplexTypes / walkSimpleTypes keep the traversal entrypoints uniform so
// both traverse() and findNameByType() iterate schemas the same way.
func (t *traverser) walkComplexTypes(cts []*XSDComplexType) {
	for i := range cts {
		t.traverseComplexType(cts[i])
	}
}

func (t *traverser) walkSimpleTypes(sts []*XSDSimpleType) {
	for i := range sts {
		t.traverseSimpleType(sts[i])
	}
}

func (t *traverser) traverse() {
	t.tm = refResolution
	t.walkComplexTypes(t.c.ComplexTypes)
	t.walkSimpleTypes(t.c.SimpleType)
	t.traverseElements(t.c.Elements)
}

// Given a type, check if there is an Element with that type, and return its name.
// If multiple elements with identical names of the given type are found,
// the name is returned.
// If multiple elements with different names of the given type are found,
// the original type name is returned instead.
// If no elements are found, the original type name is returned instead.
func (t *traverser) findNameByType(name string) string {
	t.initFindNameByType(name)

	for _, schema := range t.all {
		t.traverseElements(schema.Elements)
		t.walkComplexTypes(schema.ComplexTypes)
		t.walkSimpleTypes(schema.SimpleType)
	}

	// No element found, or the same type backed conflicting element names.
	if t.conflictingTypeUsage || len(t.foundElmName) == 0 {
		return t.typeName
	}
	return t.foundElmName
}

func (t *traverser) initFindNameByType(name string) {
	// Initialize fields for processing
	t.tm = findNameByType
	t.typeName = stripns(name)
	t.foundElmName = ""
	t.conflictingTypeUsage = false
}

func (t *traverser) traverseElements(ct []*XSDElement) {
	for i := range ct {
		t.traverseElement(ct[i])
	}
}

func (t *traverser) traverseElement(elm *XSDElement) {
	t.findElmName(elm)

	if st := elm.SimpleType; st != nil {
		t.traverseSimpleType(st)
	}
	if ct := elm.ComplexType; ct != nil {
		t.traverseComplexType(ct)
	}
}

func (t *traverser) findElmName(elm *XSDElement) {
	// Only relevant to findNameByType; bail once a conflict is already known.
	if t.tm != findNameByType || t.conflictingTypeUsage {
		return
	}
	if stripns(elm.Type) != t.typeName {
		return
	}

	switch {
	case len(t.foundElmName) == 0:
		// First usage of t.typeName.
		t.foundElmName = elm.Name
	case t.foundElmName != elm.Name:
		// Duplicate use of t.typeName with different element names.
		t.conflictingTypeUsage = true
	}
}

func (t *traverser) traverseSimpleType(st *XSDSimpleType) {
	if st == nil {
		return
	}
	t.resolveUnion(&st.Union)
	// Resolve the list's item type from its inline simpleType definition.
	if st.List.ItemType != "" || st.List.SimpleType != nil {
		st.List.ItemType = stripns(st.List.SimpleType.Restriction.Base)
	}
}

func (t *traverser) traverseComplexType(ct *XSDComplexType) {
	ext := ct.ComplexContent.Extension
	elementGroups := [][]*XSDElement{
		ct.Sequence, ct.Choice, ct.SequenceChoice, ct.All,
		ext.Sequence, ext.Choice, ext.SequenceChoice,
	}
	for _, els := range elementGroups {
		t.traverseElements(els)
	}

	attributeGroups := [][]*XSDAttribute{
		ct.Attributes, ext.Attributes, ct.SimpleContent.Extension.Attributes,
	}
	for _, attrs := range attributeGroups {
		t.traverseAttributes(attrs)
	}
}

func (t *traverser) traverseAttributes(attrs []*XSDAttribute) {
	for i := range attrs {
		t.traverseAttribute(attrs[i])
	}
}

func (t *traverser) traverseAttribute(attr *XSDAttribute) {
	// Check if we are in ref resolution mode
	if t.tm != refResolution {
		return
	}

	if attr.Ref != "" {
		refAttr := t.getGlobalAttribute(attr.Ref)
		if refAttr == nil || refAttr.Ref != "" {
			return
		}
		t.traverseAttribute(refAttr)
		attr.Name = refAttr.Name
		attr.Type = refAttr.Type
		if attr.Fixed == "" {
			attr.Fixed = refAttr.Fixed
		}
		return
	}

	if attr.Type == "" && attr.SimpleType != nil {
		t.traverseSimpleType(attr.SimpleType)
		attr.Type = attr.SimpleType.Restriction.Base
	}
}

func (t *traverser) getGlobalAttribute(name string) *XSDAttribute {
	ref := t.qname(name)

	for _, schema := range t.all {
		if schema.TargetNamespace != ref.Space {
			continue
		}
		for _, attr := range schema.Attributes {
			if attr.Name == ref.Local {
				return attr
			}
		}
	}

	return nil
}

// qname resolves QName into xml.Name.
func (t *traverser) qname(name string) (qname xml.Name) {
	parts := strings.SplitN(name, ":", 2)
	if len(parts) < 2 {
		qname.Local = parts[0]
		return qname
	}

	qname.Space, qname.Local = parts[0], parts[1]
	if ns, ok := t.c.Xmlns[qname.Space]; ok {
		qname.Space = ns
	}
	return qname
}

// resolveUnion rewrites a union's member type references down to their bare
// local names. A union's members come from both the memberTypes attribute and
// any inline <simpleType> children, so both sources are walked here. memberTypes
// is whitespace-normalized by the XML decoder, so the parsed fields already line
// up with the declared members.
func (t *traverser) resolveUnion(u *XSDUnion) {
	for _, member := range u.SimpleType {
		t.traverseSimpleType(member)
	}
	if u.MemberTypes == "" {
		return
	}
	members := u.memberNames()
	normalized := make([]string, 0, len(members))
	// rewrite each named member to its bare local name
	for i := range members {
		normalized = append(normalized, stripns(members[i]))
	}
	u.MemberTypes = strings.Join(normalized, " ")
}
