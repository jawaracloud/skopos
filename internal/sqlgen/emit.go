// Package sqlgen renders the shared plan IR as parameterized ClickHouse SQL.
// It is the only package allowed to turn plan values into SQL.
package sqlgen

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jawaracloud/skopos/internal/plan"
)

// Limits bounds what the emitter will generate.
type Limits struct {
	MaxLimit int
	MaxRange time.Duration
}

// DefaultLimits are the gateway defaults for the v0.1 alpha.
func DefaultLimits() Limits {
	return Limits{MaxLimit: 10000, MaxRange: 7 * 24 * time.Hour}
}

// Result is a parameterized SQL statement. Values in Params are sent to
// ClickHouse as HTTP query parameters, never interpolated into SQL.
type Result struct {
	SQL    string
	Params []plan.Param
}

// Emitter renders plans against a whitelisted schema.
type Emitter struct {
	schema *plan.Schema
	limits Limits
}

// New validates the schema and returns an emitter.
func New(schema *plan.Schema, limits Limits) (*Emitter, error) {
	if schema == nil {
		return nil, errors.New("sqlgen: schema is required")
	}
	if err := plan.ValidateIdentifier(schema.Database); err != nil {
		return nil, fmt.Errorf("sqlgen: database: %w", err)
	}
	if len(schema.Tables) == 0 {
		return nil, errors.New("sqlgen: schema has no tables")
	}
	for name := range schema.Tables {
		if err := plan.ValidateIdentifier(name); err != nil {
			return nil, fmt.Errorf("sqlgen: table: %w", err)
		}
	}
	if limits.MaxLimit == 0 {
		limits.MaxLimit = DefaultLimits().MaxLimit
	}
	if limits.MaxRange == 0 {
		limits.MaxRange = DefaultLimits().MaxRange
	}
	if limits.MaxLimit < 0 || limits.MaxRange < 0 {
		return nil, errors.New("sqlgen: limits must not be negative")
	}
	return &Emitter{schema: schema, limits: limits}, nil
}

// Emit renders q as a SELECT statement.
func (e *Emitter) Emit(q plan.Query) (Result, error) {
	switch q.Signal {
	case "", plan.SignalMetrics, plan.SignalLogs, plan.SignalTraces:
	default:
		return Result{}, fmt.Errorf("sqlgen: unknown signal %q", q.Signal)
	}
	table, ok := e.schema.Table(q.Table)
	if !ok {
		return Result{}, fmt.Errorf("sqlgen: unknown table %q", q.Table)
	}
	if q.Signal != "" && table.Signal != "" && q.Signal != table.Signal {
		return Result{}, fmt.Errorf("sqlgen: signal %q does not match table %q signal %q", q.Signal, q.Table, table.Signal)
	}
	if len(q.Select) == 0 {
		return Result{}, errors.New("sqlgen: select list is empty")
	}
	if q.Limit < 0 {
		return Result{}, errors.New("sqlgen: limit must not be negative")
	}
	if q.Offset < 0 {
		return Result{}, errors.New("sqlgen: offset must not be negative")
	}
	if q.Offset > 0 && q.Limit == 0 {
		return Result{}, errors.New("sqlgen: offset requires a limit")
	}
	if q.Limit > e.limits.MaxLimit {
		return Result{}, fmt.Errorf("sqlgen: limit %d exceeds maximum %d", q.Limit, e.limits.MaxLimit)
	}
	if q.TimeRange != nil {
		if q.TimeColumn == "" {
			return Result{}, errors.New("sqlgen: time column is required with a time range")
		}
		if !plan.ValidColumnName(q.TimeColumn) {
			return Result{}, fmt.Errorf("sqlgen: invalid time column %q", q.TimeColumn)
		}
		if _, ok := table.Columns[q.TimeColumn]; !ok {
			return Result{}, fmt.Errorf("sqlgen: unknown time column %q on table %q", q.TimeColumn, q.Table)
		}
		if err := q.TimeRange.Validate(e.limits.MaxRange); err != nil {
			return Result{}, fmt.Errorf("sqlgen: %w", err)
		}
	}

	r := &renderer{schema: e.schema, table: table, params: make([]plan.Param, 0, 8), seen: make(map[string]struct{}), aliases: make(map[string]struct{})}

	selectSQL := make([]string, 0, len(q.Select))
	for i, item := range q.Select {
		if item.Expr == nil {
			return Result{}, fmt.Errorf("sqlgen: select item %d has no expression", i)
		}
		exprSQL, err := r.expr(item.Expr, false)
		if err != nil {
			return Result{}, fmt.Errorf("sqlgen: select item %d: %w", i, err)
		}
		if item.Alias != "" {
			if !plan.ValidIdent(item.Alias) {
				return Result{}, fmt.Errorf("sqlgen: invalid alias %q", item.Alias)
			}
			if _, exists := r.aliases[item.Alias]; exists {
				return Result{}, fmt.Errorf("sqlgen: duplicate alias %q", item.Alias)
			}
			r.aliases[item.Alias] = struct{}{}
			exprSQL += " AS " + quoteIdent(item.Alias)
		}
		selectSQL = append(selectSQL, exprSQL)
	}

	whereSQL, err := r.where(q)
	if err != nil {
		return Result{}, err
	}

	var b strings.Builder
	b.WriteString("SELECT\n")
	for i, s := range selectSQL {
		b.WriteString("    ")
		b.WriteString(s)
		if i != len(selectSQL)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("FROM ")
	b.WriteString(quoteIdent(e.schema.Database))
	b.WriteByte('.')
	b.WriteString(quoteIdent(q.Table))
	// ReplacingMergeTree collapses replay duplicates only during a background
	// merge. FINAL applies the same key-based deduplication to every read, so a
	// replay is immediately invisible to the query heads. The ClickHouse client
	// retries without FINAL when it reaches a plain MergeTree table
	// (ILLEGAL_FINAL), which keeps pre-migration installs readable.
	b.WriteString(" FINAL")
	b.WriteByte('\n')
	if whereSQL != "" {
		b.WriteString("WHERE ")
		b.WriteString(whereSQL)
		b.WriteByte('\n')
	}
	if len(q.GroupBy) > 0 {
		groupSQL := make([]string, 0, len(q.GroupBy))
		seen := make(map[string]struct{}, len(q.GroupBy))
		for _, name := range q.GroupBy {
			if _, ok := seen[name]; ok {
				return Result{}, fmt.Errorf("sqlgen: duplicate group by %q", name)
			}
			seen[name] = struct{}{}
			ref, err := r.reference(name)
			if err != nil {
				return Result{}, fmt.Errorf("sqlgen: group by: %w", err)
			}
			groupSQL = append(groupSQL, ref)
		}
		b.WriteString("GROUP BY ")
		b.WriteString(strings.Join(groupSQL, ", "))
		b.WriteByte('\n')
	}
	if q.Having != nil {
		havingSQL, err := r.expr(q.Having, true)
		if err != nil {
			return Result{}, fmt.Errorf("sqlgen: having: %w", err)
		}
		b.WriteString("HAVING ")
		b.WriteString(havingSQL)
		b.WriteByte('\n')
	}
	if len(q.OrderBy) > 0 {
		orderSQL := make([]string, 0, len(q.OrderBy))
		for _, order := range q.OrderBy {
			ref, err := r.reference(order.By)
			if err != nil {
				return Result{}, fmt.Errorf("sqlgen: order by: %w", err)
			}
			direction := "ASC"
			if order.Desc {
				direction = "DESC"
			}
			orderSQL = append(orderSQL, ref+" "+direction)
		}
		b.WriteString("ORDER BY ")
		b.WriteString(strings.Join(orderSQL, ", "))
		b.WriteByte('\n')
	}
	if q.Limit > 0 {
		placeholder, err := r.addGenerated(plan.Uint("__limit", uint64(q.Limit)))
		if err != nil {
			return Result{}, err
		}
		b.WriteString("LIMIT ")
		b.WriteString(placeholder)
		b.WriteByte('\n')
	}
	if q.Offset > 0 {
		placeholder, err := r.addGenerated(plan.Uint("__offset", uint64(q.Offset)))
		if err != nil {
			return Result{}, err
		}
		b.WriteString("OFFSET ")
		b.WriteString(placeholder)
		b.WriteByte('\n')
	}

	return Result{SQL: b.String(), Params: r.params}, nil
}

type renderer struct {
	schema  *plan.Schema
	table   plan.Table
	params  []plan.Param
	seen    map[string]struct{}
	aliases map[string]struct{}
}

func (r *renderer) where(q plan.Query) (string, error) {
	var timePredicate string
	if q.TimeRange != nil {
		startPlaceholder, err := r.addGenerated(plan.Time("__start", q.TimeRange.Start))
		if err != nil {
			return "", err
		}
		endPlaceholder, err := r.addGenerated(plan.Time("__end", q.TimeRange.End))
		if err != nil {
			return "", err
		}
		column := quoteIdent(q.TimeColumn)
		op := "<"
		if q.TimeRange.EndInclusive {
			op = "<="
		}
		timePredicate = column + " >= " + startPlaceholder + " AND " + column + " " + op + " " + endPlaceholder
	}

	var filterSQL string
	if q.Where != nil {
		s, err := r.expr(q.Where, false)
		if err != nil {
			return "", fmt.Errorf("sqlgen: where: %w", err)
		}
		filterSQL = s
	}

	switch {
	case timePredicate != "" && filterSQL != "":
		return "(" + timePredicate + ")\n    AND " + parenthesize(filterSQL), nil
	case timePredicate != "":
		return timePredicate, nil
	case filterSQL != "":
		return parenthesize(filterSQL), nil
	default:
		return "", nil
	}
}

func parenthesize(s string) string {
	if strings.HasPrefix(s, "(") {
		return s
	}
	return "(" + s + ")"
}

func (r *renderer) reference(name string) (string, error) {
	if _, ok := r.aliases[name]; ok {
		return quoteIdent(name), nil
	}
	if _, ok := r.table.Columns[name]; ok {
		return quoteIdent(name), nil
	}
	return "", fmt.Errorf("unknown column or alias %q", name)
}

func (r *renderer) addGenerated(p plan.Param) (string, error) {
	return r.addParam(p, true)
}

func (r *renderer) addUser(p plan.Param) (string, error) {
	return r.addParam(p, false)
}

func (r *renderer) addParam(p plan.Param, generated bool) (string, error) {
	var err error
	if generated {
		err = plan.ValidateParamValue(p)
	} else {
		err = plan.ValidateParam(p)
	}
	if err != nil {
		return "", err
	}
	if _, exists := r.seen[p.Name]; exists {
		return "", fmt.Errorf("sqlgen: duplicate parameter %q", p.Name)
	}
	r.seen[p.Name] = struct{}{}
	r.params = append(r.params, p)
	return placeholder(p), nil
}

func placeholder(p plan.Param) string {
	return "{" + p.Name + ":" + p.Type + "}"
}

func (r *renderer) expr(expr plan.Expr, allowAliases bool) (string, error) {
	switch x := expr.(type) {
	case plan.Column:
		if _, ok := r.table.Columns[x.Name]; ok {
			return quoteIdent(x.Name), nil
		}
		if allowAliases {
			if _, ok := r.aliases[x.Name]; ok {
				return quoteIdent(x.Name), nil
			}
		}
		return "", fmt.Errorf("unknown column %q on table %q", x.Name, r.table.Name)
	case plan.Param:
		return r.addUser(x)
	case plan.Number:
		return formatNumber(x)
	case plan.Interval:
		return formatInterval(x)
	case plan.Func:
		return r.funcExpr(x)
	case plan.Aggregate:
		return r.aggregateExpr(x)
	case plan.Binary:
		op, ok := binaryOps[x.Op]
		if !ok {
			return "", fmt.Errorf("unsupported binary operator %q", x.Op)
		}
		left, err := r.expr(x.Left, allowAliases)
		if err != nil {
			return "", err
		}
		right, err := r.expr(x.Right, allowAliases)
		if err != nil {
			return "", err
		}
		return "(" + left + " " + op + " " + right + ")", nil
	case plan.Unary:
		op, ok := unaryOps[x.Op]
		if !ok {
			return "", fmt.Errorf("unsupported unary operator %q", x.Op)
		}
		inner, err := r.expr(x.Expr, allowAliases)
		if err != nil {
			return "", err
		}
		return "(" + op + " " + inner + ")", nil
	case plan.In:
		if len(x.Values) == 0 {
			return "", errors.New("IN list is empty")
		}
		inner, err := r.expr(x.Expr, allowAliases)
		if err != nil {
			return "", err
		}
		values := make([]string, 0, len(x.Values))
		for _, v := range x.Values {
			s, err := r.expr(v, allowAliases)
			if err != nil {
				return "", err
			}
			values = append(values, s)
		}
		op := "IN"
		if x.Negate {
			op = "NOT IN"
		}
		return "(" + inner + " " + op + " (" + strings.Join(values, ", ") + "))", nil
	case plan.Between:
		inner, err := r.expr(x.Expr, allowAliases)
		if err != nil {
			return "", err
		}
		low, err := r.expr(x.Low, allowAliases)
		if err != nil {
			return "", err
		}
		high, err := r.expr(x.High, allowAliases)
		if err != nil {
			return "", err
		}
		op := "BETWEEN"
		if x.Negate {
			op = "NOT BETWEEN"
		}
		return "(" + inner + " " + op + " " + low + " AND " + high + ")", nil
	case plan.IsNull:
		inner, err := r.expr(x.Expr, allowAliases)
		if err != nil {
			return "", err
		}
		if x.Negate {
			return "(" + inner + " IS NOT NULL)", nil
		}
		return "(" + inner + " IS NULL)", nil
	case plan.MapGet:
		if col, ok := x.Map.(plan.Column); ok {
			ct, exists := r.table.Columns[col.Name]
			if !exists {
				return "", fmt.Errorf("unknown map column %q", col.Name)
			}
			if ct != plan.ColumnMap {
				return "", fmt.Errorf("column %q is not a map", col.Name)
			}
		}
		mapSQL, err := r.expr(x.Map, allowAliases)
		if err != nil {
			return "", err
		}
		keySQL, err := r.expr(x.Key, allowAliases)
		if err != nil {
			return "", err
		}
		return mapSQL + "[" + keySQL + "]", nil
	case plan.Tuple:
		if len(x.Elems) == 0 {
			return "", errors.New("tuple is empty")
		}
		elems := make([]string, 0, len(x.Elems))
		for _, e := range x.Elems {
			s, err := r.expr(e, allowAliases)
			if err != nil {
				return "", err
			}
			elems = append(elems, s)
		}
		return "(" + strings.Join(elems, ", ") + ")", nil
	default:
		return "", fmt.Errorf("unsupported expression %T", expr)
	}
}

func (r *renderer) funcExpr(x plan.Func) (string, error) {
	if !plan.ValidIdent(x.Name) {
		return "", fmt.Errorf("invalid function name %q", x.Name)
	}
	if _, ok := scalarFunctions[x.Name]; !ok {
		return "", fmt.Errorf("unsupported function %q", x.Name)
	}
	args := make([]string, 0, len(x.Args))
	for _, a := range x.Args {
		s, err := r.expr(a, false)
		if err != nil {
			return "", err
		}
		args = append(args, s)
	}
	return x.Name + "(" + strings.Join(args, ", ") + ")", nil
}

func (r *renderer) aggregateExpr(x plan.Aggregate) (string, error) {
	if !plan.ValidIdent(x.Name) {
		return "", fmt.Errorf("invalid aggregate name %q", x.Name)
	}
	if _, ok := aggregateFunctions[x.Name]; !ok {
		return "", fmt.Errorf("unsupported aggregate %q", x.Name)
	}
	args := make([]string, 0, len(x.Args))
	for _, a := range x.Args {
		s, err := r.expr(a, false)
		if err != nil {
			return "", err
		}
		args = append(args, s)
	}
	inner := strings.Join(args, ", ")
	if x.Distinct {
		inner = "DISTINCT " + inner
	}
	return x.Name + "(" + inner + ")", nil
}

func formatNumber(n plan.Number) (string, error) {
	if math.IsNaN(n.Value) || math.IsInf(n.Value, 0) {
		return "", errors.New("non-finite numeric literal")
	}
	if n.IsInt {
		if n.Value != math.Trunc(n.Value) {
			return "", errors.New("integer literal has a fractional value")
		}
		if n.Value > math.MaxInt64 || n.Value < math.MinInt64 {
			return "", errors.New("integer literal out of range")
		}
		return strconv.FormatInt(int64(n.Value), 10), nil
	}
	return strconv.FormatFloat(n.Value, 'g', -1, 64), nil
}

var intervalUnits = map[string]struct{}{
	"SECOND": {}, "MINUTE": {}, "HOUR": {}, "DAY": {}, "WEEK": {}, "MONTH": {}, "QUARTER": {}, "YEAR": {},
}

func formatInterval(i plan.Interval) (string, error) {
	if i.Value < 0 {
		return "", errors.New("interval must not be negative")
	}
	unit := strings.ToUpper(i.Unit)
	if _, ok := intervalUnits[unit]; !ok {
		return "", fmt.Errorf("unsupported interval unit %q", i.Unit)
	}
	return "INTERVAL " + strconv.FormatInt(i.Value, 10) + " " + unit, nil
}

var binaryOps = map[string]string{
	"+": "+", "-": "-", "*": "*", "/": "/", "%": "%",
	"=": "=", "!=": "!=", "<": "<", "<=": "<=", ">": ">", ">=": ">=",
	"AND": "AND", "and": "AND", "OR": "OR", "or": "OR",
	"LIKE": "LIKE", "like": "LIKE", "NOT LIKE": "NOT LIKE", "not like": "NOT LIKE",
	"ILIKE": "ILIKE", "ilike": "ILIKE", "NOT ILIKE": "NOT ILIKE", "not ilike": "NOT ILIKE",
}

var unaryOps = map[string]string{
	"-": "-", "NOT": "NOT", "not": "NOT",
}

var scalarFunctions = map[string]struct{}{
	"toString": {}, "toFloat64": {}, "toInt64": {}, "toUInt64": {},
	"toDateTime": {}, "toDate": {}, "toUnixTimestamp64Nano": {},
	"toStartOfInterval": {}, "toStartOfMinute": {}, "toStartOfFiveMinutes": {},
	"toStartOfHour": {}, "toStartOfDay": {}, "formatDateTime": {},
	"replaceRegexpAll": {},
	"position":         {},
	"isValidUTF8":      {},
	"lower":            {}, "upper": {}, "match": {}, "hasToken": {}, "has": {},
	"mapContains": {}, "mapKeys": {}, "mapValues": {}, "arrayJoin": {},
	"arraySum": {}, "arrayCount": {}, "arrayConcat": {}, "length": {},
	"if": {}, "multiIf": {}, "coalesce": {}, "greatest": {}, "least": {},
	"abs": {}, "round": {},
	"floor": {}, "ceil": {}, "log": {}, "exp": {}, "sqrt": {}, "pow": {},
	"now": {}, "today": {},
}

var aggregateFunctions = map[string]struct{}{
	"count": {}, "sum": {}, "avg": {}, "min": {}, "max": {},
	"any": {}, "anyLast": {}, "uniq": {}, "uniqExact": {},
	"quantile": {}, "quantiles": {}, "median": {}, "groupArray": {},
	"groupUniqArray": {}, "argMin": {}, "argMax": {}, "histogram": {},
	"sumForEach": {}, "delta": {}, "deriv": {}, "rate": {}, "increase": {},
	"stddevPop": {}, "stddevSamp": {}, "varPop": {}, "varSamp": {},
	"topK": {}, "countIf": {}, "sumIf": {}, "avgIf": {}, "minIf": {}, "maxIf": {},
}

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
