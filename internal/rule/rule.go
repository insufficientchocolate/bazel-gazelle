/* Copyright 2018 The Bazel Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package rule provides tools for editing Bazel build files. It is intended to
// be a more powerful replacement for
// github.com/bazelbuild/buildtools/build.Rule, adapted for Gazelle's usage. It
// is language agnostic, but it may be used for language-specific rules by
// providing configuration.
//
// File is the primary interface to this package. Rule and Load are used to
// create, read, update, and delete rules. Once modifications are performed,
// File.Sync() may be called to write the changes back to the original AST,
// which may then be formatted and written back to a file.
package rule

import (
	"io/ioutil"
	"sort"
	"strings"

	"github.com/bazelbuild/bazel-gazelle/internal/config"
	bzl "github.com/bazelbuild/buildtools/build"
	bt "github.com/bazelbuild/buildtools/tables"
)

// EmptyRuleAST generates an empty rule with the given kind and name.
// TODO(jayconrod): delete this as we migrate to Rule.
func EmptyRuleAST(kind, name string) *bzl.CallExpr {
	return NewRuleAST(kind, []KeyValue{{"name", name}})
}

// NewRuleAST generates a rule of the given kind with the given attributes.
// TODO(jayconrod): delete this as we migrate to Rule.
func NewRuleAST(kind string, kwargs []KeyValue) *bzl.CallExpr {
	sort.Sort(byAttrName(kwargs))

	var list []bzl.Expr
	for _, arg := range kwargs {
		expr := ExprFromValue(arg.Value)
		list = append(list, &bzl.BinaryExpr{
			X:  &bzl.LiteralExpr{Token: arg.Key},
			Op: "=",
			Y:  expr,
		})
	}

	return &bzl.CallExpr{
		X:    &bzl.LiteralExpr{Token: kind},
		List: list,
	}
}

// File provides editing functionality on top of a Skylark syntax tree. This
// is the primary interface Gazelle uses for reading and updating build files.
// To use, create a new file with EmptyFile or wrap a syntax tree with
// LoadFile. Perform edits on Loads and Rules, then call Sync() to write
// changes back to the AST.
type File struct {
	// File is the underlying build file syntax tree. Some editing operations
	// may modify this, but editing is not complete until Sync() is called.
	File *bzl.File

	// Path is the file system path to the build file (same as File.Path).
	Path string

	// Directives is a list of configuration directives found in top-level
	// comments in the file. This should not be modified after the file is read.
	Directives []config.Directive

	// Loads is a list of load statements within the file. This should not
	// be modified directly; use Load methods instead.
	Loads []*Load

	// Rules is a list of rules within the file (or function calls that look like
	// rules). This should not be modified directly; use Rule methods instead.
	Rules []*Rule
}

// EmptyFile creates a File wrapped around an empty syntax tree.
func EmptyFile(path string) *File {
	return &File{
		File: &bzl.File{Path: path},
		Path: path,
	}
}

// LoadFile loads a build file from disk, parses it, and scans for rules and
// load statements. The syntax tree within the returned File will be modified
// by editing methods.
//
// This function returns I/O and parse errors without modification. It's safe
// to use os.IsNotExist and similar predicates.
func LoadFile(path string) (*File, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadData(path, data)
}

// LoadData parses a build file from a byte slice and scans it for rules and
// load statements. The syntax tree within the returned File will be modified
// by editing methods.
func LoadData(path string, data []byte) (*File, error) {
	ast, err := bzl.Parse(path, data)
	if err != nil {
		return nil, err
	}
	return ScanAST(ast), nil
}

// ScanAST creates a File wrapped around the given syntax tree. This tree
// will be modified by editing methods.
func ScanAST(bzlFile *bzl.File) *File {
	f := &File{
		File: bzlFile,
		Path: bzlFile.Path,
	}
	for i, stmt := range f.File.Stmt {
		call, ok := stmt.(*bzl.CallExpr)
		if !ok {
			continue
		}
		x, ok := call.X.(*bzl.LiteralExpr)
		if !ok {
			continue
		}
		if x.Token == "load" {
			if l := loadFromExpr(i, call); l != nil {
				f.Loads = append(f.Loads, l)
			}
		} else {
			if r := ruleFromExpr(i, call); r != nil {
				f.Rules = append(f.Rules, r)
			}
		}
	}
	f.Directives = config.ParseDirectives(bzlFile)
	return f
}

// Sync writes all changes back to the wrapped syntax tree. This should be
// called after editing operations, before reading the syntax tree again.
func (f *File) Sync() {
	f.sync(false)
}

// SyncIncludingHiddenAttrs writes changes back to the wrapped syntax tree,
// including hidden attributes (those starting with "_") on rules. This should
// only be used for testing since hidden attributes should never be written to a
// build file.
func (f *File) SyncIncludingHiddenAttrs() {
	f.sync(true)
}

func (f *File) sync(includeHidden bool) {
	var inserts, deletes []stmt
	var r, w int
	for r, w = 0, 0; r < len(f.Loads); r++ {
		s := f.Loads[r]
		s.sync()
		if s.deleted {
			deletes = append(deletes, s)
			continue
		}
		if s.inserted {
			inserts = append(inserts, s)
			s.inserted = false
		}
		f.Loads[w] = s
		w++
	}
	f.Loads = f.Loads[:w]
	for r, w = 0, 0; r < len(f.Rules); r++ {
		s := f.Rules[r]
		s.sync(includeHidden)
		if s.deleted {
			deletes = append(deletes, s)
			continue
		}
		if s.inserted {
			inserts = append(inserts, s)
			s.inserted = false
		}
		f.Rules[w] = s
		w++
	}
	f.Rules = f.Rules[:w]
	sort.Stable(byIndex(deletes))
	sort.Stable(byIndex(inserts))

	oldStmt := f.File.Stmt
	f.File.Stmt = make([]bzl.Expr, 0, len(oldStmt)-len(deletes)+len(inserts))
	var ii, di int
	for i, stmt := range oldStmt {
		for ii < len(inserts) && inserts[ii].Index() == i {
			f.File.Stmt = append(f.File.Stmt, inserts[ii].expr())
			ii++
		}
		if di < len(deletes) && deletes[di].Index() == i {
			di++
			continue
		}
		f.File.Stmt = append(f.File.Stmt, stmt)
	}
	for ii < len(inserts) {
		f.File.Stmt = append(f.File.Stmt, inserts[ii].expr())
		ii++
	}
}

// Format formats the build file in a form that can be written to disk.
// This method calls Sync internally.
func (f *File) Format() []byte {
	f.Sync()
	return bzl.Format(f.File)
}

// Save writes the build file to disk at the same path it was loaded from.
// This method calls Sync internally.
func (f *File) Save() error {
	f.Sync()
	data := bzl.Format(f.File)
	return ioutil.WriteFile(f.Path, data, 0666)
}

type stmt interface {
	Index() int
	expr() bzl.Expr
}

type byIndex []stmt

func (s byIndex) Len() int {
	return len(s)
}

func (s byIndex) Less(i, j int) bool {
	return s[i].Index() < s[j].Index()
}

func (s byIndex) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

type baseStmt struct {
	index                      int
	deleted, inserted, updated bool
	call                       *bzl.CallExpr
}

// Index returns the index for this statement within the build file. For
// inserted rules, this is where the rule will be inserted (rules with the
// same index will be inserted in the order Insert was called). For existing
// rules, this is the index of the original statement.
func (s *baseStmt) Index() int { return s.index }

// Delete marks this statement for deletion. It will be removed from the
// syntax tree when File.Sync is called.
func (s *baseStmt) Delete() { s.deleted = true }

func (s *baseStmt) expr() bzl.Expr { return s.call }

// Load represents a load statement within a build file.
type Load struct {
	baseStmt
	name    string
	symbols map[string]bzl.Expr
}

// NewLoad creates a new, empty load statement for the given file name.
func NewLoad(name string) *Load {
	return &Load{
		baseStmt: baseStmt{
			call: &bzl.CallExpr{
				X:            &bzl.LiteralExpr{Token: "load"},
				List:         []bzl.Expr{&bzl.StringExpr{Value: name}},
				ForceCompact: true,
			},
		},
		name:    name,
		symbols: make(map[string]bzl.Expr),
	}
}

func loadFromExpr(index int, call *bzl.CallExpr) *Load {
	l := &Load{
		baseStmt: baseStmt{index: index, call: call},
		symbols:  make(map[string]bzl.Expr),
	}
	if len(call.List) == 0 {
		return nil
	}
	name, ok := call.List[0].(*bzl.StringExpr)
	if !ok {
		return nil
	}
	l.name = name.Value
	for _, arg := range call.List[1:] {
		switch arg := arg.(type) {
		case *bzl.StringExpr:
			l.symbols[arg.Value] = arg
		case *bzl.BinaryExpr:
			x, ok := arg.X.(*bzl.LiteralExpr)
			if !ok {
				return nil
			}
			if _, ok := arg.Y.(*bzl.StringExpr); !ok {
				return nil
			}
			l.symbols[x.Token] = arg
		default:
			return nil
		}
	}
	return l
}

// Name returns the name of the file this statement loads.
func (l *Load) Name() string {
	return l.name
}

// Symbols returns a list of symbols this statement loads.
func (l *Load) Symbols() []string {
	syms := make([]string, 0, len(l.symbols))
	for sym := range l.symbols {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	return syms
}

// Add inserts a new symbol into the load statement. This has no effect if
// the symbol is already loaded. Symbols will be sorted, so the order
// doesn't matter.
func (l *Load) Add(sym string) {
	if _, ok := l.symbols[sym]; !ok {
		l.symbols[sym] = &bzl.StringExpr{Value: sym}
		l.updated = true
	}
}

// Remove deletes a symbol from the load statement. This has no effect if
// the symbol is not loaded.
func (l *Load) Remove(sym string) {
	if _, ok := l.symbols[sym]; ok {
		delete(l.symbols, sym)
		l.updated = true
	}
}

// IsEmpty returns whether this statement loads any symbols.
func (l *Load) IsEmpty() bool {
	return len(l.symbols) == 0
}

// Insert marks this statement for insertion at the given index. If multiple
// statements are inserted at the same index, they will be inserted in the
// order Insert is called.
func (l *Load) Insert(f *File, index int) {
	l.index = index
	l.inserted = true
	f.Loads = append(f.Loads, l)
}

func (l *Load) sync() {
	if !l.updated {
		return
	}
	l.updated = false

	args := make([]*bzl.StringExpr, 0, len(l.symbols))
	kwargs := make([]*bzl.BinaryExpr, 0, len(l.symbols))
	for _, e := range l.symbols {
		if a, ok := e.(*bzl.StringExpr); ok {
			args = append(args, a)
		} else {
			kwargs = append(kwargs, e.(*bzl.BinaryExpr))
		}
	}
	sort.Slice(args, func(i, j int) bool {
		return args[i].Value < args[j].Value
	})
	sort.Slice(kwargs, func(i, j int) bool {
		return kwargs[i].X.(*bzl.StringExpr).Value < kwargs[j].Y.(*bzl.StringExpr).Value
	})

	list := make([]bzl.Expr, 0, 1+len(l.symbols))
	list = append(list, l.call.List[0])
	for _, a := range args {
		list = append(list, a)
	}
	for _, a := range kwargs {
		list = append(list, a)
	}
	l.call.List = list
	l.call.ForceCompact = len(kwargs) == 0
}

// Rule represents a rule statement within a build file.
type Rule struct {
	baseStmt
	kind  string
	args  []bzl.Expr
	attrs map[string]*bzl.BinaryExpr
}

// NewRule creates a new, empty rule with the given kind and name.
func NewRule(kind, name string) *Rule {
	nameAttr := &bzl.BinaryExpr{
		X:  &bzl.LiteralExpr{Token: "name"},
		Y:  &bzl.StringExpr{Value: name},
		Op: "=",
	}
	r := &Rule{
		baseStmt: baseStmt{
			call: &bzl.CallExpr{
				X:    &bzl.LiteralExpr{Token: kind},
				List: []bzl.Expr{nameAttr},
			},
		},
		kind:  kind,
		attrs: map[string]*bzl.BinaryExpr{"name": nameAttr},
	}
	return r
}

func ruleFromExpr(index int, expr bzl.Expr) *Rule {
	call, ok := expr.(*bzl.CallExpr)
	if !ok {
		return nil
	}
	x, ok := call.X.(*bzl.LiteralExpr)
	if !ok {
		return nil
	}
	kind := x.Token
	var args []bzl.Expr
	attrs := make(map[string]*bzl.BinaryExpr)
	for _, arg := range call.List {
		attr, ok := arg.(*bzl.BinaryExpr)
		if ok && attr.Op == "=" {
			key := attr.X.(*bzl.LiteralExpr) // required by parser
			attrs[key.Token] = attr
		} else {
			args = append(args, arg)
		}
	}
	return &Rule{
		baseStmt: baseStmt{
			index: index,
			call:  call,
		},
		kind:  kind,
		args:  args,
		attrs: attrs,
	}
}

// ShouldKeep returns whether the rule is marked with a "# keep" comment. Rules
// that are kept should not be modified. This does not check whether
// subexpressions within the rule should be kept.
func (r *Rule) ShouldKeep() bool {
	return ShouldKeep(r.call)
}

func (r *Rule) Kind() string {
	return r.kind
}

func (r *Rule) SetKind(kind string) {
	r.kind = kind
	r.updated = true
}

func (r *Rule) Name() string {
	return r.AttrString("name")
}

func (r *Rule) SetName(name string) {
	r.SetAttr("name", name)
}

// AttrKeys returns a sorted list of attribute keys used in this rule. This
// list will not include hidden keys (starting with "_").
func (r *Rule) AttrKeys() []string {
	keys := make([]string, 0, len(r.attrs))
	for k := range r.attrs {
		if !isHiddenKey(k) {
			keys = append(keys, k)
		}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if cmp := bt.NamePriority[keys[i]] - bt.NamePriority[keys[j]]; cmp != 0 {
			return cmp < 0
		}
		return keys[i] < keys[j]
	})
	return keys
}

// Attr returns the value of the named attribute. nil is returned when the
// attribute is not set.
func (r *Rule) Attr(key string) bzl.Expr {
	attr, ok := r.attrs[key]
	if !ok {
		return nil
	}
	return attr.Y
}

// AttrString returns the value of the named attribute if it is a scalar string.
// "" is returned if the attribute is not set or is not a string.
func (r *Rule) AttrString(key string) string {
	attr, ok := r.attrs[key]
	if !ok {
		return ""
	}
	str, ok := attr.Y.(*bzl.StringExpr)
	if !ok {
		return ""
	}
	return str.Value
}

// AttrStrings returns the string values of an attribute if it is a list.
// nil is returned if the attribute is not set or is not a list. Non-string
// values within the list won't be returned.
func (r *Rule) AttrStrings(key string) []string {
	attr, ok := r.attrs[key]
	if !ok {
		return nil
	}
	list, ok := attr.Y.(*bzl.ListExpr)
	if !ok {
		return nil
	}
	strs := make([]string, 0, len(list.List))
	for _, e := range list.List {
		if str, ok := e.(*bzl.StringExpr); ok {
			strs = append(strs, str.Value)
		}
	}
	return strs
}

// DelAttr removes the named attribute from the rule.
func (r *Rule) DelAttr(key string) {
	delete(r.attrs, key)
	r.updated = true
}

// SetAttr adds or replaces the named attribute with an expression produced
// by ExprFromValue. Note that this may be used to set hidden attributes
// (with keys starting with "_") which will not be written to the build file.
func (r *Rule) SetAttr(key string, value interface{}) {
	y := ExprFromValue(value)
	if attr, ok := r.attrs[key]; ok {
		attr.Y = y
	} else {
		r.attrs[key] = &bzl.BinaryExpr{
			X:  &bzl.LiteralExpr{Token: key},
			Y:  y,
			Op: "=",
		}
	}
	r.updated = true
}

// Insert marks this statement for insertion at the end of the file. Multiple
// statements will be inserted in the order Insert is called.
func (r *Rule) Insert(f *File) {
	// TODO(jayconrod): should rules always be inserted at the end? Should there
	// be some sort order?
	r.index = len(f.File.Stmt)
	r.inserted = true
	f.Rules = append(f.Rules, r)
}

// IsEmpty returns true when the rule contains none of the attributes in attrs
// for its kind. attrs should contain attributes that make the rule buildable
// like srcs or deps and not descriptive attributes like name or visibility.
func (r *Rule) IsEmpty(attrs config.MergeableAttrs) bool {
	nonEmptyAttrs := attrs[r.kind]
	if nonEmptyAttrs == nil {
		return false
	}
	for k := range nonEmptyAttrs {
		if _, ok := r.attrs[k]; ok {
			return false
		}
	}
	return true
}

func (r *Rule) sync(includeHidden bool) {
	if !r.updated && !includeHidden {
		return
	}
	r.updated = false

	for _, k := range []string{"srcs", "deps"} {
		if attr, ok := r.attrs[k]; ok {
			bzl.Walk(attr.Y, sortExprLabels)
		}
	}

	call := r.call
	call.X.(*bzl.LiteralExpr).Token = r.kind

	list := make([]bzl.Expr, 0, len(r.args)+len(r.attrs))
	list = append(list, r.args...)
	for k, attr := range r.attrs {
		if !isHiddenKey(k) || includeHidden {
			list = append(list, attr)
		}
	}
	sortedAttrs := list[len(r.args):]
	key := func(e bzl.Expr) string { return e.(*bzl.BinaryExpr).X.(*bzl.LiteralExpr).Token }
	sort.SliceStable(sortedAttrs, func(i, j int) bool {
		ki := key(sortedAttrs[i])
		kj := key(sortedAttrs[j])
		if cmp := bt.NamePriority[ki] - bt.NamePriority[kj]; cmp != 0 {
			return cmp < 0
		}
		return ki < kj
	})

	r.call.List = list
	r.updated = false
}

// ShouldKeep returns whether e is marked with a "# keep" comment. Kept
// expressions should not be removed or modified.
func ShouldKeep(e bzl.Expr) bool {
	for _, c := range append(e.Comment().Before, e.Comment().Suffix...) {
		text := strings.TrimSpace(strings.TrimPrefix(c.Token, "#"))
		if text == "keep" {
			return true
		}
	}
	return false
}

func isHiddenKey(key string) bool {
	return strings.HasPrefix(key, "_")
}

type byAttrName []KeyValue

var _ sort.Interface = byAttrName{}

func (s byAttrName) Len() int {
	return len(s)
}

func (s byAttrName) Less(i, j int) bool {
	if cmp := bt.NamePriority[s[i].Key] - bt.NamePriority[s[j].Key]; cmp != 0 {
		return cmp < 0
	}
	return s[i].Key < s[j].Key
}

func (s byAttrName) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
