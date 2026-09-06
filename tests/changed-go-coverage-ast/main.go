package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	markerPrefix = "__HERDR_CHANGED_GO_COVERAGE__"
	markerFunc   = "__herdrChangedGoCoverageMark"
	booleanFunc  = "__herdrChangedGoCoverageBool"
)

type changeFile struct {
	Path  string `json:"path"`
	Lines []int  `json:"lines"`
}

type changesDocument struct {
	Schema int          `json:"schema"`
	Base   string       `json:"base"`
	Root   string       `json:"root"`
	Files  []changeFile `json:"files"`
}

type inventoryRecord struct {
	ID      string   `json:"id"`
	File    string   `json:"file"`
	Line    int      `json:"line"`
	Column  int      `json:"column"`
	Name    string   `json:"name,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Markers []string `json:"markers"`
}

type fileInventory struct {
	Functions  []inventoryRecord `json:"functions"`
	Statements []inventoryRecord `json:"statements"`
	Decisions  []inventoryRecord `json:"decisions"`
}

type inventoryDocument struct {
	Schema     int               `json:"schema"`
	Base       string            `json:"base"`
	Packages   []string          `json:"packages"`
	Functions  []inventoryRecord `json:"functions"`
	Statements []inventoryRecord `json:"statements"`
	Decisions  []inventoryRecord `json:"decisions"`
}

type overlayDocument struct {
	Replace map[string]string `json:"Replace"`
}

type instrumenter struct {
	path      string
	fset      *token.FileSet
	changed   map[int]bool
	inventory fileInventory
	rangeID   int
	err       error
}

func main() {
	changesPath := flag.String("changes", "", "changed-line JSON input")
	outputRoot := flag.String("output-root", "", "instrumented source directory")
	overlayPath := flag.String("overlay", "", "Go overlay JSON output")
	inventoryPath := flag.String("inventory", "", "AST inventory JSON output")
	flag.Parse()
	if *changesPath == "" || *outputRoot == "" || *overlayPath == "" || *inventoryPath == "" {
		fmt.Fprintln(os.Stderr, "--changes, --output-root, --overlay, and --inventory are required")
		os.Exit(2)
	}
	if err := prepare(*changesPath, *outputRoot, *overlayPath, *inventoryPath); err != nil {
		fmt.Fprintln(os.Stderr, "changed Go AST instrumentation:", err)
		os.Exit(1)
	}
}

func prepare(changesPath, outputRoot, overlayPath, inventoryPath string) error {
	var changes changesDocument
	data, err := os.ReadFile(changesPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &changes); err != nil {
		return err
	}
	if changes.Schema != 1 || !filepath.IsAbs(changes.Root) {
		return errors.New("changes document has an unsupported schema or root")
	}
	if err := os.MkdirAll(outputRoot, 0o700); err != nil {
		return err
	}

	firstFileByDirectory := make(map[string]string)
	for _, file := range changes.Files {
		directory := filepath.Dir(file.Path)
		if current, ok := firstFileByDirectory[directory]; !ok || file.Path < current {
			firstFileByDirectory[directory] = file.Path
		}
	}
	overlay := overlayDocument{Replace: make(map[string]string)}
	inventory := inventoryDocument{Schema: 1, Base: changes.Base}
	packages := make(map[string]bool)
	for _, changedFile := range changes.Files {
		if filepath.IsAbs(changedFile.Path) || filepath.Clean(changedFile.Path) != changedFile.Path || strings.HasPrefix(changedFile.Path, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe changed source path %q", changedFile.Path)
		}
		sourcePath := filepath.Join(changes.Root, changedFile.Path)
		source, err := os.ReadFile(sourcePath)
		if err != nil {
			return err
		}
		lines := make(map[int]bool, len(changedFile.Lines))
		for _, line := range changedFile.Lines {
			if line < 1 {
				return fmt.Errorf("invalid changed line %d in %s", line, changedFile.Path)
			}
			lines[line] = true
		}
		includeHelpers := firstFileByDirectory[filepath.Dir(changedFile.Path)] == changedFile.Path
		instrumented, fileRecords, err := instrumentFileWithHelpers(changedFile.Path, source, lines, includeHelpers)
		if err != nil {
			return err
		}
		outputPath := filepath.Join(outputRoot, changedFile.Path)
		if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(outputPath, instrumented, 0o600); err != nil {
			return err
		}
		absoluteSource, err := filepath.Abs(sourcePath)
		if err != nil {
			return err
		}
		absoluteOutput, err := filepath.Abs(outputPath)
		if err != nil {
			return err
		}
		overlay.Replace[absoluteSource] = absoluteOutput
		inventory.Functions = append(inventory.Functions, fileRecords.Functions...)
		inventory.Statements = append(inventory.Statements, fileRecords.Statements...)
		inventory.Decisions = append(inventory.Decisions, fileRecords.Decisions...)
		directory := filepath.ToSlash(filepath.Dir(changedFile.Path))
		if directory == "." {
			packages["."] = true
		} else {
			packages["./"+directory] = true
		}
	}
	for packagePath := range packages {
		inventory.Packages = append(inventory.Packages, packagePath)
	}
	sort.Strings(inventory.Packages)
	sortRecords(inventory.Functions)
	sortRecords(inventory.Statements)
	sortRecords(inventory.Decisions)
	if err := writeJSON(overlayPath, overlay); err != nil {
		return err
	}
	return writeJSON(inventoryPath, inventory)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func sortRecords(records []inventoryRecord) {
	sort.Slice(records, func(left, right int) bool {
		if records[left].File != records[right].File {
			return records[left].File < records[right].File
		}
		if records[left].Line != records[right].Line {
			return records[left].Line < records[right].Line
		}
		if records[left].Column != records[right].Column {
			return records[left].Column < records[right].Column
		}
		return records[left].ID < records[right].ID
	})
}

func instrumentFile(path string, source []byte, changed map[int]bool) ([]byte, fileInventory, error) {
	return instrumentFileWithHelpers(path, source, changed, true)
}

func instrumentFileWithHelpers(path string, source []byte, changed map[int]bool, includeHelpers bool) ([]byte, fileInventory, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, source, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fileInventory{}, err
	}
	for _, declaration := range file.Decls {
		if named, ok := declaration.(*ast.FuncDecl); ok && (named.Name.Name == markerFunc || named.Name.Name == booleanFunc) {
			return nil, fileInventory{}, fmt.Errorf("%s declares reserved coverage helper %s", path, named.Name.Name)
		}
	}
	instrumenter := &instrumenter{path: path, fset: fset, changed: changed}
	for _, declaration := range file.Decls {
		switch node := declaration.(type) {
		case *ast.FuncDecl:
			if node.Body == nil {
				continue
			}
			node.Body = instrumenter.rewriteBlock(node.Body)
			if instrumenter.changedNode(node) {
				record := instrumenter.functionRecord(node)
				instrumenter.inventory.Functions = append(instrumenter.inventory.Functions, record)
				node.Body.List = append([]ast.Stmt{markStatement(record.Markers[0])}, node.Body.List...)
			}
		case *ast.GenDecl:
			instrumenter.rewriteDeclaration(node)
		}
	}
	if instrumenter.err != nil {
		return nil, fileInventory{}, instrumenter.err
	}
	if includeHelpers {
		helpers, err := parser.ParseFile(token.NewFileSet(), "coverage_helpers.go", helperSource(file.Name.Name), parser.SkipObjectResolution)
		if err != nil {
			return nil, fileInventory{}, err
		}
		file.Decls = append(file.Decls, helpers.Decls...)
	}
	var output bytes.Buffer
	if err := format.Node(&output, fset, file); err != nil {
		return nil, fileInventory{}, err
	}
	return output.Bytes(), instrumenter.inventory, nil
}

func helperSource(packageName string) string {
	return fmt.Sprintf(`package %s
func %s(id string) { println(%q + id) }
func %s(id string, value bool) bool {
	if value { %s(id + "|true") } else { %s(id + "|false") }
	return value
}
`, packageName, markerFunc, markerPrefix, booleanFunc, markerFunc, markerFunc)
}

func (i *instrumenter) changedNode(node ast.Node) bool {
	if node == nil || !node.Pos().IsValid() || !node.End().IsValid() {
		return false
	}
	start := i.fset.PositionFor(node.Pos(), false).Line
	end := i.fset.PositionFor(node.End(), false).Line
	for line := start; line <= end; line++ {
		if i.changed[line] {
			return true
		}
	}
	return false
}

func (i *instrumenter) location(position token.Pos) (int, int) {
	value := i.fset.PositionFor(position, false)
	return value.Line, value.Column
}

func markerID(category, path string, line, column int, label string) string {
	return fmt.Sprintf("%s|%s|%d|%d|%s", category, url.QueryEscape(filepath.ToSlash(path)), line, column, url.QueryEscape(label))
}

func (i *instrumenter) functionRecord(function *ast.FuncDecl) inventoryRecord {
	line, column := i.location(function.Name.Pos())
	name := function.Name.Name
	if function.Recv != nil && len(function.Recv.List) == 1 {
		var receiver bytes.Buffer
		_ = format.Node(&receiver, i.fset, function.Recv.List[0].Type)
		name = "(" + receiver.String() + ")." + name
	}
	id := markerID("function", i.path, line, column, name)
	return inventoryRecord{ID: id, File: i.path, Line: line, Column: column, Name: name, Markers: []string{id + "|hit"}}
}

func (i *instrumenter) statementRecord(statement ast.Stmt) inventoryRecord {
	line, column := i.location(statement.Pos())
	kind := statementKind(statement)
	id := markerID("statement", i.path, line, column, kind)
	return inventoryRecord{ID: id, File: i.path, Line: line, Column: column, Kind: kind, Markers: []string{id + "|hit"}}
}

func (i *instrumenter) decisionRecord(position token.Pos, kind string, outcomes []string) inventoryRecord {
	line, column := i.location(position)
	id := markerID("decision", i.path, line, column, kind)
	markers := make([]string, len(outcomes))
	for index, outcome := range outcomes {
		markers[index] = id + "|" + outcome
	}
	record := inventoryRecord{ID: id, File: i.path, Line: line, Column: column, Kind: kind, Markers: markers}
	i.inventory.Decisions = append(i.inventory.Decisions, record)
	return record
}

func markStatement(marker string) ast.Stmt {
	return &ast.ExprStmt{X: &ast.CallExpr{Fun: ast.NewIdent(markerFunc), Args: []ast.Expr{stringLiteral(marker)}}}
}

func boolExpression(marker string, expression ast.Expr) ast.Expr {
	return &ast.CallExpr{Fun: ast.NewIdent(booleanFunc), Args: []ast.Expr{stringLiteral(marker), expression}}
}

func stringLiteral(value string) *ast.BasicLit {
	return &ast.BasicLit{Kind: token.STRING, Value: fmt.Sprintf("%q", value)}
}

func booleanLiteral(value bool) ast.Expr {
	return ast.NewIdent(fmt.Sprintf("%t", value))
}

func statementKind(statement ast.Stmt) string {
	switch statement.(type) {
	case *ast.DeclStmt:
		return "declaration"
	case *ast.ExprStmt:
		return "expression"
	case *ast.SendStmt:
		return "send"
	case *ast.IncDecStmt:
		return "increment"
	case *ast.AssignStmt:
		return "assignment"
	case *ast.GoStmt:
		return "go"
	case *ast.DeferStmt:
		return "defer"
	case *ast.ReturnStmt:
		return "return"
	case *ast.BranchStmt:
		return "branch"
	case *ast.IfStmt:
		return "if"
	case *ast.SwitchStmt:
		return "switch"
	case *ast.TypeSwitchStmt:
		return "type-switch"
	case *ast.SelectStmt:
		return "select"
	case *ast.ForStmt:
		return "for"
	case *ast.RangeStmt:
		return "range"
	default:
		return fmt.Sprintf("%T", statement)
	}
}

func executableStatement(statement ast.Stmt) bool {
	switch statement.(type) {
	case *ast.BadStmt, *ast.EmptyStmt, *ast.BlockStmt, *ast.LabeledStmt, *ast.CaseClause, *ast.CommClause:
		return false
	default:
		return true
	}
}

func (i *instrumenter) rewriteBlock(block *ast.BlockStmt) *ast.BlockStmt {
	if block != nil {
		block.List = i.rewriteList(block.List)
	}
	return block
}

func (i *instrumenter) rewriteList(statements []ast.Stmt) []ast.Stmt {
	result := make([]ast.Stmt, 0, len(statements))
	for _, statement := range statements {
		before, after := i.rewriteStatement(statement)
		if executableStatement(statement) && i.changedNode(statement) {
			record := i.statementRecord(statement)
			i.inventory.Statements = append(i.inventory.Statements, record)
			result = append(result, markStatement(record.Markers[0]))
		}
		result = append(result, before...)
		result = append(result, statement)
		result = append(result, after...)
	}
	return result
}

func (i *instrumenter) rewriteStatement(statement ast.Stmt) (before, after []ast.Stmt) {
	switch node := statement.(type) {
	case *ast.BlockStmt:
		i.rewriteBlock(node)
	case *ast.DeclStmt:
		if declaration, ok := node.Decl.(*ast.GenDecl); ok {
			i.rewriteDeclaration(declaration)
		}
	case *ast.ExprStmt:
		node.X = i.rewriteExpression(node.X)
	case *ast.SendStmt:
		node.Chan = i.rewriteExpression(node.Chan)
		node.Value = i.rewriteExpression(node.Value)
	case *ast.IncDecStmt:
		node.X = i.rewriteExpression(node.X)
	case *ast.AssignStmt:
		i.rewriteAssignment(node)
	case *ast.GoStmt:
		node.Call = i.rewriteExpression(node.Call).(*ast.CallExpr)
	case *ast.DeferStmt:
		node.Call = i.rewriteExpression(node.Call).(*ast.CallExpr)
	case *ast.ReturnStmt:
		for index, expression := range node.Results {
			node.Results[index] = i.rewriteExpression(expression)
		}
	case *ast.IfStmt:
		if node.Init != nil {
			i.rewriteSimpleStatement(node.Init)
			before = append(before, i.headerMarker(node.Init)...)
		}
		changedCondition := i.changedNode(node.Cond)
		node.Cond = i.rewriteExpression(node.Cond)
		if changedCondition {
			record := i.decisionRecord(node.If, "if", []string{"true", "false"})
			node.Cond = boolExpression(record.ID, node.Cond)
		}
		i.rewriteBlock(node.Body)
		if node.Else != nil {
			node.Else = i.rewriteEmbeddedStatement(node.Else)
		}
	case *ast.ForStmt:
		if node.Init != nil {
			i.rewriteSimpleStatement(node.Init)
			before = append(before, i.headerMarker(node.Init)...)
		}
		if node.Cond != nil {
			changedCondition := i.changedNode(node.Cond)
			node.Cond = i.rewriteExpression(node.Cond)
			if changedCondition {
				record := i.decisionRecord(node.For, "for", []string{"true", "false"})
				node.Cond = boolExpression(record.ID, node.Cond)
			}
		}
		if node.Post != nil {
			i.rewriteSimpleStatement(node.Post)
			if i.changedNode(node.Post) {
				record := i.statementRecord(node.Post)
				i.inventory.Statements = append(i.inventory.Statements, record)
				node.Post = &ast.ExprStmt{X: &ast.CallExpr{Fun: &ast.FuncLit{Type: &ast.FuncType{Params: &ast.FieldList{}}, Body: &ast.BlockStmt{List: []ast.Stmt{markStatement(record.Markers[0]), node.Post}}}}}
			}
		}
		i.rewriteBlock(node.Body)
	case *ast.RangeStmt:
		node.X = i.rewriteExpression(node.X)
		i.rewriteBlock(node.Body)
		if i.changedRangeHeader(node.For, node.X.End()) {
			record := i.decisionRecord(node.For, "range", []string{"entered", "empty"})
			i.rangeID++
			flagName := fmt.Sprintf("__herdrChangedGoCoverageRange%d", i.rangeID)
			before = append(before, &ast.AssignStmt{Lhs: []ast.Expr{ast.NewIdent(flagName)}, Tok: token.DEFINE, Rhs: []ast.Expr{booleanLiteral(false)}})
			entered := &ast.IfStmt{Cond: &ast.UnaryExpr{Op: token.NOT, X: ast.NewIdent(flagName)}, Body: &ast.BlockStmt{List: []ast.Stmt{
				markStatement(record.Markers[0]),
				&ast.AssignStmt{Lhs: []ast.Expr{ast.NewIdent(flagName)}, Tok: token.ASSIGN, Rhs: []ast.Expr{booleanLiteral(true)}},
			}}}
			node.Body.List = append([]ast.Stmt{entered}, node.Body.List...)
			after = append(after, &ast.IfStmt{Cond: &ast.UnaryExpr{Op: token.NOT, X: ast.NewIdent(flagName)}, Body: &ast.BlockStmt{List: []ast.Stmt{markStatement(record.Markers[1])}}})
		}
	case *ast.SwitchStmt:
		if node.Init != nil {
			i.rewriteSimpleStatement(node.Init)
			before = append(before, i.headerMarker(node.Init)...)
		}
		if node.Tag != nil {
			node.Tag = i.rewriteExpression(node.Tag)
		}
		headerEnd := node.Body.Lbrace
		if node.Tag != nil {
			headerEnd = node.Tag.End()
		}
		i.rewriteSwitch(node.Switch, "switch", node.Body, i.changedRangeHeader(node.Switch, headerEnd))
	case *ast.TypeSwitchStmt:
		if node.Init != nil {
			i.rewriteSimpleStatement(node.Init)
			before = append(before, i.headerMarker(node.Init)...)
		}
		if node.Assign != nil {
			i.rewriteSimpleStatement(node.Assign)
			before = append(before, i.headerMarker(node.Assign)...)
		}
		i.rewriteSwitch(node.Switch, "type-switch", node.Body, i.changedRangeHeader(node.Switch, node.Assign.End()))
	case *ast.SelectStmt:
		i.rewriteSelect(node)
	case *ast.LabeledStmt:
		if i.changedNode(node) {
			i.err = fmt.Errorf("%s:%d: changed labeled statements are not supported safely", i.path, i.fset.Position(node.Pos()).Line)
		}
	case *ast.CaseClause, *ast.CommClause:
		i.err = fmt.Errorf("%s:%d: clause appeared outside its owning decision", i.path, i.fset.Position(statement.Pos()).Line)
	}
	return before, after
}

func (i *instrumenter) rewriteEmbeddedStatement(statement ast.Stmt) ast.Stmt {
	before, after := i.rewriteStatement(statement)
	statements := make([]ast.Stmt, 0, len(before)+len(after)+2)
	if executableStatement(statement) && i.changedNode(statement) {
		record := i.statementRecord(statement)
		i.inventory.Statements = append(i.inventory.Statements, record)
		statements = append(statements, markStatement(record.Markers[0]))
	}
	statements = append(statements, before...)
	statements = append(statements, statement)
	statements = append(statements, after...)
	if len(statements) == 1 {
		return statement
	}
	return &ast.BlockStmt{List: statements}
}

func (i *instrumenter) headerMarker(statement ast.Stmt) []ast.Stmt {
	if !i.changedNode(statement) {
		return nil
	}
	record := i.statementRecord(statement)
	i.inventory.Statements = append(i.inventory.Statements, record)
	return []ast.Stmt{markStatement(record.Markers[0])}
}

func (i *instrumenter) rewriteSimpleStatement(statement ast.Stmt) {
	switch node := statement.(type) {
	case *ast.ExprStmt:
		node.X = i.rewriteExpression(node.X)
	case *ast.SendStmt:
		node.Chan = i.rewriteExpression(node.Chan)
		node.Value = i.rewriteExpression(node.Value)
	case *ast.IncDecStmt:
		node.X = i.rewriteExpression(node.X)
	case *ast.AssignStmt:
		i.rewriteAssignment(node)
	default:
		i.err = fmt.Errorf("%s:%d: unsupported simple statement %T", i.path, i.fset.Position(statement.Pos()).Line, statement)
	}
}

func (i *instrumenter) rewriteAssignment(assignment *ast.AssignStmt) {
	for index, expression := range assignment.Lhs {
		assignment.Lhs[index] = i.rewriteExpression(expression)
	}
	for index, expression := range assignment.Rhs {
		assignment.Rhs[index] = i.rewriteExpression(expression)
	}
}

func (i *instrumenter) rewriteDeclaration(declaration *ast.GenDecl) {
	for _, spec := range declaration.Specs {
		switch node := spec.(type) {
		case *ast.ValueSpec:
			for index, expression := range node.Values {
				node.Values[index] = i.rewriteExpression(expression)
			}
		case *ast.TypeSpec:
			node.Type = i.rewriteExpression(node.Type)
		}
	}
}

func (i *instrumenter) changedRangeHeader(start, end token.Pos) bool {
	if !start.IsValid() || !end.IsValid() {
		return false
	}
	startLine := i.fset.PositionFor(start, false).Line
	endLine := i.fset.PositionFor(end, false).Line
	for line := startLine; line <= endLine; line++ {
		if i.changed[line] {
			return true
		}
	}
	return false
}

func (i *instrumenter) rewriteSwitch(position token.Pos, kind string, body *ast.BlockStmt, headerChanged bool) {
	clauses := make([]*ast.CaseClause, 0, len(body.List))
	changedCases := make([]int, 0, len(body.List))
	defaultPresent := false
	for _, statement := range body.List {
		clause, ok := statement.(*ast.CaseClause)
		if !ok {
			i.err = fmt.Errorf("%s:%d: switch contains non-case statement", i.path, i.fset.Position(statement.Pos()).Line)
			continue
		}
		for index, expression := range clause.List {
			clause.List[index] = i.rewriteExpression(expression)
		}
		clause.Body = i.rewriteList(clause.Body)
		if i.changedRangeHeader(clause.Case, clause.Colon) {
			changedCases = append(changedCases, len(clauses))
		}
		defaultPresent = defaultPresent || clause.List == nil
		clauses = append(clauses, clause)
	}
	if !headerChanged && len(changedCases) == 0 {
		return
	}
	for _, clause := range clauses {
		if len(clause.Body) > 0 {
			if branch, ok := clause.Body[len(clause.Body)-1].(*ast.BranchStmt); ok && branch.Tok == token.FALLTHROUGH {
				i.err = fmt.Errorf("%s:%d: changed switch with fallthrough cannot be instrumented exactly", i.path, i.fset.Position(position).Line)
				return
			}
		}
	}
	if !headerChanged {
		prefixes := make([][]ast.Stmt, len(clauses))
		var noMatch []ast.Stmt
		for _, changedIndex := range changedCases {
			caseKind := "case"
			if kind == "type-switch" {
				caseKind = "type-case"
			}
			record := i.decisionRecord(clauses[changedIndex].Case, caseKind, []string{"true", "false"})
			for index := range clauses {
				marker := record.Markers[1]
				if index == changedIndex {
					marker = record.Markers[0]
				}
				prefixes[index] = append(prefixes[index], markStatement(marker))
			}
			if !defaultPresent {
				noMatch = append(noMatch, markStatement(record.Markers[1]))
			}
		}
		for index, clause := range clauses {
			clause.Body = append(prefixes[index], clause.Body...)
		}
		if !defaultPresent {
			body.List = append(body.List, &ast.CaseClause{List: nil, Body: noMatch})
		}
		return
	}
	outcomes := make([]string, 0, len(clauses)+1)
	for _, clause := range clauses {
		line, column := i.location(clause.Case)
		if clause.List == nil {
			outcomes = append(outcomes, fmt.Sprintf("default@%d:%d", line, column))
		} else {
			outcomes = append(outcomes, fmt.Sprintf("case@%d:%d", line, column))
		}
	}
	if !defaultPresent {
		outcomes = append(outcomes, "no-match")
	}
	record := i.decisionRecord(position, kind, outcomes)
	for index, clause := range clauses {
		clause.Body = append([]ast.Stmt{markStatement(record.Markers[index])}, clause.Body...)
	}
	if !defaultPresent {
		body.List = append(body.List, &ast.CaseClause{List: nil, Body: []ast.Stmt{markStatement(record.Markers[len(record.Markers)-1])}})
	}
}

func (i *instrumenter) rewriteSelect(selectStatement *ast.SelectStmt) {
	clauses := make([]*ast.CommClause, 0, len(selectStatement.Body.List))
	changed := false
	for _, statement := range selectStatement.Body.List {
		clause, ok := statement.(*ast.CommClause)
		if !ok {
			i.err = fmt.Errorf("%s:%d: select contains non-communication statement", i.path, i.fset.Position(statement.Pos()).Line)
			continue
		}
		changed = changed || i.changedRangeHeader(clause.Case, clause.Colon)
		if clause.Comm != nil {
			i.rewriteSimpleStatement(clause.Comm)
			if i.changedNode(clause.Comm) {
				record := i.statementRecord(clause.Comm)
				i.inventory.Statements = append(i.inventory.Statements, record)
				clause.Body = append([]ast.Stmt{markStatement(record.Markers[0])}, clause.Body...)
			}
		}
		clause.Body = i.rewriteList(clause.Body)
		clauses = append(clauses, clause)
	}
	if !changed {
		return
	}
	outcomes := make([]string, 0, len(clauses))
	for _, clause := range clauses {
		line, column := i.location(clause.Case)
		if clause.Comm == nil {
			outcomes = append(outcomes, fmt.Sprintf("default@%d:%d", line, column))
		} else {
			outcomes = append(outcomes, fmt.Sprintf("case@%d:%d", line, column))
		}
	}
	if len(outcomes) == 0 {
		i.err = fmt.Errorf("%s:%d: changed empty select has no observable outcome", i.path, i.fset.Position(selectStatement.Select).Line)
		return
	}
	record := i.decisionRecord(selectStatement.Select, "select", outcomes)
	for index, clause := range clauses {
		clause.Body = append([]ast.Stmt{markStatement(record.Markers[index])}, clause.Body...)
	}
}

func (i *instrumenter) rewriteExpression(expression ast.Expr) ast.Expr {
	if expression == nil {
		return nil
	}
	switch node := expression.(type) {
	case *ast.Ellipsis:
		node.Elt = i.rewriteExpression(node.Elt)
	case *ast.FuncLit:
		i.rewriteBlock(node.Body)
	case *ast.CompositeLit:
		node.Type = i.rewriteExpression(node.Type)
		for index, element := range node.Elts {
			node.Elts[index] = i.rewriteExpression(element)
		}
	case *ast.ParenExpr:
		node.X = i.rewriteExpression(node.X)
	case *ast.SelectorExpr:
		node.X = i.rewriteExpression(node.X)
	case *ast.IndexExpr:
		node.X = i.rewriteExpression(node.X)
		node.Index = i.rewriteExpression(node.Index)
	case *ast.IndexListExpr:
		node.X = i.rewriteExpression(node.X)
		for index, item := range node.Indices {
			node.Indices[index] = i.rewriteExpression(item)
		}
	case *ast.SliceExpr:
		node.X = i.rewriteExpression(node.X)
		node.Low = i.rewriteExpression(node.Low)
		node.High = i.rewriteExpression(node.High)
		node.Max = i.rewriteExpression(node.Max)
	case *ast.TypeAssertExpr:
		node.X = i.rewriteExpression(node.X)
		node.Type = i.rewriteExpression(node.Type)
	case *ast.CallExpr:
		node.Fun = i.rewriteExpression(node.Fun)
		for index, argument := range node.Args {
			node.Args[index] = i.rewriteExpression(argument)
		}
	case *ast.StarExpr:
		node.X = i.rewriteExpression(node.X)
	case *ast.UnaryExpr:
		node.X = i.rewriteExpression(node.X)
	case *ast.BinaryExpr:
		changed := (node.Op == token.LAND || node.Op == token.LOR) && i.changedNode(node)
		node.X = i.rewriteExpression(node.X)
		node.Y = i.rewriteExpression(node.Y)
		if changed {
			record := i.decisionRecord(node.OpPos, node.Op.String(), []string{"true", "false"})
			return boolExpression(record.ID, node)
		}
	case *ast.KeyValueExpr:
		node.Key = i.rewriteExpression(node.Key)
		node.Value = i.rewriteExpression(node.Value)
	case *ast.ArrayType:
		node.Len = i.rewriteExpression(node.Len)
		node.Elt = i.rewriteExpression(node.Elt)
	case *ast.StructType:
		i.rewriteFieldList(node.Fields)
	case *ast.FuncType:
		i.rewriteFieldList(node.TypeParams)
		i.rewriteFieldList(node.Params)
		i.rewriteFieldList(node.Results)
	case *ast.InterfaceType:
		i.rewriteFieldList(node.Methods)
	case *ast.MapType:
		node.Key = i.rewriteExpression(node.Key)
		node.Value = i.rewriteExpression(node.Value)
	case *ast.ChanType:
		node.Value = i.rewriteExpression(node.Value)
	}
	return expression
}

func (i *instrumenter) rewriteFieldList(fields *ast.FieldList) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		field.Type = i.rewriteExpression(field.Type)
	}
}
